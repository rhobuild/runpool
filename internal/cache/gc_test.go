package cache

import (
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// gcFixture builds lanes with controlled ages and sizes. Ages are set
// directly in the store because LastUsed is the LRU clock under test.
func gcFixture(t *testing.T) (*LaneManager, *store.Store, *fakeVolumes) {
	m, st, vols := newManager(t)
	vols.sizes = map[string]int64{}
	return m, st, vols
}

func addLane(t *testing.T, m *LaneManager, st *store.Store, vols *fakeVolumes, repo, gen, lease string, ageDays int, size int64) LaneMount {
	t.Helper()
	loc, ok, err := m.Acquire(t.Context(), repo, gen, assignment.LeaseID(lease), 10)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v, %v", ok, err)
	}
	vols.sizes[loc.Volume] = size
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		if err := tx.BackdateCacheLane(loc.LaneID, time.Duration(ageDays)*day); err != nil {
			return err
		}
		return tx.ReleaseCacheLane(assignment.LeaseID(lease))
	}); err != nil {
		t.Fatal(err)
	}
	return loc
}

const day = 24 * time.Hour

// TestGCPlanTTL: free lanes idle past the TTL go, fresh ones stay, and
// the plan is computed against the caller's clock so it is reproducible.
func TestGCPlanTTL(t *testing.T) {
	m, st, vols := gcFixture(t)
	old := addLane(t, m, st, vols, repoURL, "gen", "l1", 30, 100)
	fresh := addLane(t, m, st, vols, "https://github.com/acme/other", "gen", "l2", 1, 100)

	plan, err := m.PlanGC(t.Context(), GCOptions{TTL: 14 * day, TargetBytes: -1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Evictions) != 1 || plan.Evictions[0].LaneID != old.LaneID || plan.Evictions[0].Reason != "ttl" {
		t.Fatalf("plan = %+v; want exactly the idle lane by ttl", plan.Evictions)
	}
	if plan.ManagedBytes != 200 || plan.KeptBytes != 100 {
		t.Errorf("managed=%d kept=%d; want 200/100", plan.ManagedBytes, plan.KeptBytes)
	}
	_ = fresh
}

// TestGCPlanLRUTowardTarget: over the watermark, the oldest free lanes
// go first and eviction stops at the target — deterministically, so the
// same facts always produce the same plan.
func TestGCPlanLRUTowardTarget(t *testing.T) {
	m, st, vols := gcFixture(t)
	oldest := addLane(t, m, st, vols, repoURL, "g1", "l1", 9, 400)
	middle := addLane(t, m, st, vols, repoURL, "g2", "l2", 5, 400)
	newest := addLane(t, m, st, vols, repoURL, "g3", "l3", 1, 400)

	plan, err := m.PlanGC(t.Context(), GCOptions{TargetBytes: 500, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Evictions) != 2 {
		t.Fatalf("evictions = %+v; want the two oldest", plan.Evictions)
	}
	if plan.Evictions[0].LaneID != oldest.LaneID || plan.Evictions[1].LaneID != middle.LaneID {
		t.Errorf("order = %s, %s; want oldest %s then %s", plan.Evictions[0].LaneID, plan.Evictions[1].LaneID, oldest.LaneID, middle.LaneID)
	}
	if plan.KeptBytes != 400 {
		t.Errorf("kept = %d; want 400", plan.KeptBytes)
	}
	_ = newest
}

// TestGCNeverPlansALeasedLane: whatever the pressure, a lane a job is
// writing to is untouchable.
func TestGCNeverPlansALeasedLane(t *testing.T) {
	m, _, vols := gcFixture(t)
	loc, ok, err := m.Acquire(t.Context(), repoURL, "gen", "holder", 1)
	if err != nil || !ok {
		t.Fatal(err)
	}
	vols.sizes[loc.Volume] = 1 << 30

	// Aggressive posture: every free lane goes — and the leased one
	// still stays.
	plan, err := m.PlanGC(t.Context(), GCOptions{TTL: time.Nanosecond, AllFree: true, Now: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Evictions) != 0 {
		t.Fatalf("a leased lane was planned for eviction: %+v", plan.Evictions)
	}
}

// TestGCPlansOrphanVolumes: a labeled lane volume without a row is
// unreachable by any lease and only this sweep can find it.
func TestGCPlansOrphanVolumes(t *testing.T) {
	m, st, vols := gcFixture(t)
	loc := addLane(t, m, st, vols, repoURL, "gen", "l1", 1, 50)

	// Simulate the DeleteLane crash window: row gone, volume left.
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.DeleteCacheLane(loc.LaneID)
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := m.PlanGC(t.Context(), GCOptions{TargetBytes: -1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Evictions) != 1 || !plan.Evictions[0].Orphan || plan.Evictions[0].Volume != loc.Volume {
		t.Fatalf("plan = %+v; want the orphan volume", plan.Evictions)
	}

	res, err := m.RunGC(t.Context(), plan, "test")
	if err != nil || res.Applied != 1 || len(res.Failed) != 0 {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if len(vols.removed) != 1 || vols.removed[0] != loc.Volume {
		t.Errorf("removed = %v; want the orphan", vols.removed)
	}
}

// TestRunGCAuditsAndSkipsRaced: every applied eviction leaves an audit
// entry; a lane leased between plan and apply is skipped, not failed.
func TestRunGCAuditsAndSkipsRaced(t *testing.T) {
	m, st, vols := gcFixture(t)
	victim := addLane(t, m, st, vols, repoURL, "g1", "l1", 30, 100)
	raced := addLane(t, m, st, vols, repoURL, "g2", "l2", 30, 100)

	plan, err := m.PlanGC(t.Context(), GCOptions{TTL: day, TargetBytes: -1, Now: time.Now()})
	if err != nil || len(plan.Evictions) != 2 {
		t.Fatalf("plan = %+v, %v", plan.Evictions, err)
	}

	// A job takes one lane between plan and apply.
	if _, ok, err := m.Acquire(t.Context(), repoURL, "g2", "late-lease", 10); err != nil || !ok {
		t.Fatal(err)
	}

	res, err := m.RunGC(t.Context(), plan, "test-actor")
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 || res.Skipped != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v; want 1 applied, 1 skipped", res)
	}
	if len(vols.removed) != 1 || vols.removed[0] != victim.Volume {
		t.Errorf("removed = %v; want only %q", vols.removed, victim.Volume)
	}

	var audits []store.AuditEntry
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		audits, err = tx.AuditTail(10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Actor != "test-actor" || audits[0].Action != "gc_evict" || audits[0].Subject != victim.Volume {
		t.Fatalf("audit = %+v; want one gc_evict for %q", audits, victim.Volume)
	}
	_ = raced
}

// TestGCTarget pins the formula that used to exist in two places — the
// serving monitor's and the operator's `gc`. The invariant the second one
// documents is that a gc run must not invent thresholds the controller
// would not use, and two copies of an arithmetic is how one of them
// eventually does.
func TestGCTarget(t *testing.T) {
	const budget = 100 << 30 // 100 GiB
	for name, tc := range map[string]struct {
		max    int64
		lowPct int
		want   int64
	}{
		"low watermark of the budget": {budget, 70, 70 << 30},
		"no budget, nothing to keep":  {0, 70, 0},
		"a zero low mark empties":     {budget, 0, 0},
		"a full low mark keeps all":   {budget, 100, budget},
	} {
		if got := GCTarget(tc.max, tc.lowPct); got != tc.want {
			t.Errorf("%s: GCTarget(%d, %d) = %d; want %d",
				name, tc.max, tc.lowPct, got, tc.want)
		}
	}
}

// TestAggressiveCollectionPlansLanesTheDaemonCannotSize: a soft emergency
// asks for every free lane, and a lane whose volume the daemon reports no
// usage data for measures as zero. Sizing the pass by bytes would find a
// zero total already under any ceiling and plan nothing, in the one
// posture that exists to reclaim everything.
func TestAggressiveCollectionPlansLanesTheDaemonCannotSize(t *testing.T) {
	m, st, vols := gcFixture(t)
	// Size zero is what the measurement floors an unsizeable volume to.
	addLane(t, m, st, vols, repoURL+"-a", "gen", "lease-a", 1, 0)
	addLane(t, m, st, vols, repoURL+"-b", "gen", "lease-b", 1, 0)
	addLane(t, m, st, vols, repoURL+"-c", "gen", "lease-c", 1, 0)

	plan, err := m.PlanGC(t.Context(), GCOptions{AllFree: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ManagedBytes != 0 {
		t.Fatalf("fixture is not the case under test: managed = %d, want 0", plan.ManagedBytes)
	}
	if len(plan.Evictions) != 3 {
		t.Fatalf("planned %d evictions for 3 unsized free lanes: %+v",
			len(plan.Evictions), plan.Evictions)
	}
	for _, e := range plan.Evictions {
		if e.Reason != "emergency" {
			t.Errorf("lane %s evicted as %q, want emergency", e.LaneID, e.Reason)
		}
	}
}
