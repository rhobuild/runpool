package command

import (
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"
)

// TestExcludeSparesTheInstancesOwnObjects pins the rule the three limbs of
// a sweep have to agree on: an object with no lease belongs to the
// instance, not to lease garbage.
//
// The containers limb is the one that needs a second condition. A
// lease-less network or volume is persistent — the uplink, a cache lane —
// so it is never garbage. A lease-less container is a helper the instance
// starts for itself, and RunTask removes its own on a deadline of its
// own: one still running is in flight, and taking it away fails the
// measurement that started it. One that is stopped outlived the process
// that owned it, and collecting it is the whole point of the sweep.
func TestExcludeSparesTheInstancesOwnObjects(t *testing.T) {
	plan := ownedPlan{
		instanceID: "inst-1",
		containers: []engine.OwnedContainer{
			{ID: "c1", Name: "runpool-inst-1-df-probe-aa", Role: "probe", Running: true},
			{ID: "c2", Name: "runpool-inst-1-hostnet-probe-bb", Role: "probe", Running: false},
			{ID: "c3", Name: "runner-live", Role: "capsule", LeaseID: "lse-live", Running: true},
			{ID: "c4", Name: "runner-done", Role: "capsule", LeaseID: "lse-done"},
		},
		networks: []engine.OwnedResource{
			{ID: "n1", Role: "uplink"},
			{ID: "n2", Role: "capsule-net", LeaseID: "lse-done"},
		},
		volumes: []engine.OwnedResource{
			{ID: "v1", Role: "cache-lane"},
			{ID: "v2", Role: "dind-data", LeaseID: "lse-done"},
		},
	}

	got := plan.exclude(map[assignment.LeaseID]bool{"lse-live": true})

	want := map[string]bool{"c2": true, "c4": true, "n2": true, "v2": true}
	for _, id := range collectIDs(got) {
		if !want[id] {
			t.Errorf("exclude offered %s for removal; it is not lease garbage", id)
		}
		delete(want, id)
	}
	for id := range want {
		t.Errorf("exclude withheld %s; it is garbage a cleanup has to collect", id)
	}
}

func collectIDs(p ownedPlan) []string {
	out := make([]string, 0, len(p.containers)+len(p.networks)+len(p.volumes))
	for _, c := range p.containers {
		out = append(out, c.ID)
	}
	for _, n := range p.networks {
		out = append(out, n.ID)
	}
	for _, v := range p.volumes {
		out = append(out, v.ID)
	}
	return out
}

// TestLiveLeaseCount: the snapshot carries live leases whole and finished
// ones bounded, so counting the slice would report the bound. Only the
// live half can be counted from it.
func TestLiveLeaseCount(t *testing.T) {
	snap := store.Snapshot{Leases: []store.Lease{
		{ID: "a", State: store.LeaseWorkloadRunning},
		{ID: "b", State: store.LeaseQuarantined},
		{ID: "c", State: store.LeaseReleased},
		{ID: "d", State: store.LeaseCleaning},
	}}
	if got := liveLeaseCount(snap); got != 3 {
		t.Errorf("liveLeaseCount = %d; want the three non-terminal leases", got)
	}
	if got := liveLeaseCount(store.Snapshot{}); got != 0 {
		t.Errorf("liveLeaseCount of an empty snapshot = %d; want 0", got)
	}
}

// TestDescribeNamesEveryObjectItWouldTake. This string is the whole
// preview: an operator reads it and decides whether to pass --apply, so
// an object missing from it is one removed without being announced.
func TestDescribeNamesEveryObjectItWouldTake(t *testing.T) {
	plan := ownedPlan{
		instanceID: "inst-1",
		containers: []engine.OwnedContainer{{ID: "c1", Name: "runner-1", Role: "capsule"}},
		networks:   []engine.OwnedResource{{ID: "n1", Role: "capsule-net"}},
		volumes:    []engine.OwnedResource{{ID: "v1", Role: "dind-data"}},
	}
	if plan.empty() {
		t.Fatal("a plan with three objects reported itself empty")
	}
	got := plan.describe("would remove", false)
	for _, want := range []string{"runner-1", "n1", "v1", "would remove"} {
		if !strings.Contains(got, want) {
			t.Errorf("the preview does not name %q:\n%s", want, got)
		}
	}
	// Applying says so in the present tense; a preview that reads like an
	// action already taken is worse than no preview.
	if applying := plan.describe("would remove", true); !strings.Contains(applying, "removing") {
		t.Errorf("the applying form still reads as a preview:\n%s", applying)
	}
	if (ownedPlan{}).empty() != true {
		t.Error("an empty plan did not report itself empty")
	}
}

// TestQueuedAttemptsAreVisibleBeforeAPurge. What uninstall counts out is
// what an operator weighs before destroying the books, and it counted
// leases. A queued attempt has none: it is work the provider assigned and
// this instance acknowledged, waiting for a lane. An instance whose lanes
// are idle under disk pressure, or whose binding has stalled, holds a
// backlog and reports zero — so the confirmation an operator reads before
// the purge is the one shape where the number has to be right.
func TestQueuedAttemptsAreVisibleBeforeAPurge(t *testing.T) {
	backlog := store.Snapshot{Queued: map[int64]int{1: 4, 2: 2}}

	if got := liveLeaseCount(backlog); got != 0 {
		t.Fatalf("liveLeaseCount = %d; this case is the one where no lease is live", got)
	}
	if got := queuedAttemptCount(backlog.Queued); got != 6 {
		t.Errorf("queuedAttemptCount = %d; want the sum across bindings, 6 — the purge "+
			"destroys every one of them", got)
	}
	if got := queuedAttemptCount(nil); got != 0 {
		t.Errorf("queuedAttemptCount of nothing queued = %d; want 0", got)
	}
}

// TestUninstallRefusesBeforeItDescribes: a wrong confirmation is
// refused before anything is described in the present tense. The
// refusal-first order matters because the description reads "removing"
// once a confirmation is present — a command that prints the removal
// line and then refuses has told the operator something happened that
// did not.
func TestUninstallRefusesBeforeItDescribes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPOOL_STATE_DIR", dir)
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, stdout, stderr := run(t, "uninstall", "--confirm", "not-this-instance")
	if code == exitOK {
		t.Fatal("a wrong confirmation succeeded")
	}
	if !strings.Contains(stderr, "does not name this instance") {
		t.Errorf("the refusal does not say what was wrong: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("the refusal printed a description first:\n%s", stdout)
	}
}
