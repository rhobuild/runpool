package cache

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// fakeVolumes stands in for the daemon: it remembers what was ensured
// with which labels, and what was removed. The real EnsureOwnedVolume
// and removal semantics are proven against a live daemon in
// test/contract/cache; here the subject is the manager's own logic.
type fakeVolumes struct {
	ensured   map[string]map[string]string
	sizes     map[string]int64
	removed   []string
	ensureErr error
	foreign   bool
}

func (f *fakeVolumes) EnsureOwnedVolume(_ context.Context, name string, labels map[string]string) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	if f.ensured == nil {
		f.ensured = map[string]map[string]string{}
	}
	f.ensured[name] = labels
	return nil
}

func (f *fakeVolumes) OwnedIDByName(_ context.Context, kind, name string, instanceID assignment.InstanceID, leaseID assignment.LeaseID) (string, error) {
	if f.foreign {
		return "", docker.ErrForeignResource
	}
	if _, ok := f.ensured[name]; !ok {
		return "", nil
	}
	return name, nil
}

func (f *fakeVolumes) RemoveVolume(_ context.Context, name string) error {
	delete(f.ensured, name)
	f.removed = append(f.removed, name)
	return nil
}

// sizes lets a GC test weigh volumes; unset names report unknown (-1),
// like a daemon that could not compute a size.
func (f *fakeVolumes) OwnedVolumeUsage(context.Context, assignment.InstanceID) ([]docker.VolumeUsage, error) {
	var out []docker.VolumeUsage
	for name, labels := range f.ensured {
		size := int64(-1)
		if f.sizes != nil {
			if s, ok := f.sizes[name]; ok {
				size = s
			}
		}
		out = append(out, docker.VolumeUsage{Name: name, Labels: labels, Size: size})
	}
	return out, nil
}

func newManager(t *testing.T) (*LaneManager, *store.Store, *fakeVolumes) {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vols := &fakeVolumes{}
	return New(st, vols, st.InstanceID()), st, vols
}

const repoURL = "https://github.com/acme/app"

// release frees a lease's lane the way production does: inside a store
// transaction, where the finalizing commit runs it alongside the lease's
// own release.
func release(t *testing.T, st *store.Store, leaseID assignment.LeaseID) error {
	t.Helper()
	return st.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.ReleaseCacheLane(assignment.LeaseID(leaseID))
	})
}

// TestLaneIsReusedAcrossLeases: a second job for the same repository and
// generation must receive the same lane — the same volume name — so the
// warm data left by the first is what it mounts. That the data itself
// survives inside the volume is the live daemon's contract test.
func TestLaneIsReusedAcrossLeases(t *testing.T) {
	m, store, _ := newManager(t)

	first, ok, err := m.Acquire(t.Context(), repoURL, "rust-v1", "lease-1", 1)
	if err != nil || !ok {
		t.Fatalf("first acquire = %+v, ok=%v, %v", first, ok, err)
	}
	if err := release(t, store, "lease-1"); err != nil {
		t.Fatal(err)
	}

	second, ok, err := m.Acquire(t.Context(), repoURL, "rust-v1", "lease-2", 1)
	if err != nil || !ok {
		t.Fatalf("second acquire = %+v, ok=%v, %v", second, ok, err)
	}
	if second.LaneID != first.LaneID {
		t.Fatalf("second lease got lane %q; want the first lane %q reused", second.LaneID, first.LaneID)
	}
	if second.Volume != first.Volume || second.Volume != VolumeName(first.LaneID) {
		t.Fatalf("volume changed between leases: %q then %q", first.Volume, second.Volume)
	}
}

// TestLaneIsExclusiveWhileLeased: a lane in use is never handed to a
// second job, because two writers would corrupt the cache. With the pool
// exhausted the caller is told to run without one.
func TestLaneIsExclusiveWhileLeased(t *testing.T) {
	m, _, _ := newManager(t)

	if _, ok, err := m.Acquire(t.Context(), repoURL, "gen", "lease-1", 1); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v, %v", ok, err)
	}
	loc, ok, err := m.Acquire(t.Context(), repoURL, "gen", "lease-2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("a leased lane was handed to a second job: %+v", loc)
	}
}

// TestDifferentRepositoriesAndGenerationsAreSeparate: cache is only safe
// because a lane belongs to one repository and one generation.
func TestDifferentRepositoriesAndGenerationsAreSeparate(t *testing.T) {
	m, _, _ := newManager(t)

	a, _, err := m.Acquire(t.Context(), repoURL, "gen-1", "lease-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := m.Acquire(t.Context(), "https://github.com/acme/other", "gen-1", "lease-b", 2)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := m.Acquire(t.Context(), repoURL, "gen-2", "lease-c", 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Volume == b.Volume || a.Volume == c.Volume || b.Volume == c.Volume {
		t.Errorf("lanes collided across repository or generation: %q %q %q", a.Volume, b.Volume, c.Volume)
	}
	if a.ProjectID == b.ProjectID {
		t.Error("two repositories share one opaque id")
	}
}

// TestVolumeNamesAreOpaque: repository text is attacker-influenced and
// must never reach the volume namespace; the name derives only from the
// lane's minted id. Identity travels in labels, as opaque ids.
func TestVolumeNamesAreOpaque(t *testing.T) {
	m, _, vols := newManager(t)

	hostile := "https://github.com/acme/..%2f..%2fetc"
	loc, ok, err := m.Acquire(t.Context(), hostile, "gen", "lease-1", 1)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v, %v", ok, err)
	}
	if loc.Volume != VolumeName(loc.LaneID) {
		t.Errorf("volume %q is not the lane's deterministic name %q", loc.Volume, VolumeName(loc.LaneID))
	}
	if strings.Contains(loc.Volume, "acme") || strings.Contains(loc.Volume, "..") || strings.Contains(loc.Volume, "/") {
		t.Errorf("repository text reached the volume name: %q", loc.Volume)
	}
	labels := vols.ensured[loc.Volume]
	if labels == nil {
		t.Fatalf("the lane volume was never ensured; ensured = %v", vols.ensured)
	}
	if labels[LabelProject] != loc.ProjectID || labels[LabelGen] != "gen" || labels[LabelLane] != loc.LaneID {
		t.Errorf("identity labels wrong: %v", labels)
	}
	if strings.Contains(labels[LabelProject], "acme") {
		t.Errorf("repository text reached the labels: %q", labels[LabelProject])
	}
	if labels[docker.LabelManaged] != "true" || labels[docker.LabelRole] != RoleCacheLane {
		t.Errorf("ownership labels wrong: %v", labels)
	}
}

// TestEnsureFailureReturnsTheLane: a lane lease must not outlive a
// volume that could not be provided — the job runs uncached and the
// lane is immediately available to the next acquire.
func TestEnsureFailureReturnsTheLane(t *testing.T) {
	m, _, vols := newManager(t)

	vols.ensureErr = errors.New("daemon unreachable")
	if _, ok, err := m.Acquire(t.Context(), repoURL, "gen", "lease-1", 1); err == nil || ok {
		t.Fatalf("acquire with a failing daemon: ok=%v, err=%v; want an error", ok, err)
	}

	vols.ensureErr = nil
	loc, ok, err := m.Acquire(t.Context(), repoURL, "gen", "lease-2", 1)
	if err != nil || !ok {
		t.Fatalf("the failed acquire did not return its lane: ok=%v, %v", ok, err)
	}
	if loc.Volume == "" {
		t.Error("no volume after recovery")
	}
}

// TestReleaseIsIdempotent: cleanup may retry, and a lease that holds no
// lane must not disturb one that does.
func TestReleaseIsIdempotent(t *testing.T) {
	m, store, _ := newManager(t)

	held, _, err := m.Acquire(t.Context(), repoURL, "gen", "holder", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(t, store, "never-held-a-lane"); err != nil {
		t.Errorf("releasing an unheld lane: %v", err)
	}
	if err := release(t, store, "holder"); err != nil {
		t.Fatal(err)
	}
	if err := release(t, store, "holder"); err != nil {
		t.Errorf("second release: %v", err)
	}
	again, ok, err := m.Acquire(t.Context(), repoURL, "gen", "next", 2)
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v, %v", ok, err)
	}
	if again.LaneID != held.LaneID {
		t.Errorf("the released lane was not reused: %q then %q", held.LaneID, again.LaneID)
	}
}

// TestLaneLeaseSurvivesRestart: the lane lease is durable state. After
// a controller restart the lane is still held by its lease — recovery
// decides its fate with the lease, and a restart must never hand a
// possibly-active lane to a new job.
func TestLaneLeaseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, &fakeVolumes{}, st.InstanceID())
	if _, ok, err := m.Acquire(t.Context(), repoURL, "gen", "holder", 1); err != nil || !ok {
		t.Fatalf("acquire: ok=%v, %v", ok, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	m2 := New(st2, &fakeVolumes{}, st2.InstanceID())
	if loc, ok, err := m2.Acquire(t.Context(), repoURL, "gen", "other", 1); err != nil || ok {
		t.Fatalf("after restart the held lane was handed out: %+v, ok=%v, %v", loc, ok, err)
	}
	if err := st2.Tx(t.Context(), func(tx *store.Tx) error { return tx.ReleaseCacheLane("holder") }); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m2.Acquire(t.Context(), repoURL, "gen", "other", 1); err != nil || !ok {
		t.Fatalf("after release the lane is still unavailable: ok=%v, %v", ok, err)
	}
}

// TestDeleteLaneRemovesOnlyThatLane: eviction deletes the row first —
// so the id can never be leased again — and then exactly one volume.
func TestDeleteLaneRemovesOnlyThatLane(t *testing.T) {
	m, store, vols := newManager(t)

	a, _, err := m.Acquire(t.Context(), repoURL, "gen", "lease-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := m.Acquire(t.Context(), "https://github.com/acme/other", "gen", "lease-b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(t, store, "lease-a"); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteLane(t.Context(), a.LaneID); err != nil {
		t.Fatal(err)
	}
	if len(vols.removed) != 1 || vols.removed[0] != a.Volume {
		t.Errorf("removed = %v; want exactly %q", vols.removed, a.Volume)
	}
	if _, ok := vols.ensured[b.Volume]; !ok {
		t.Errorf("the other lane's volume went with it")
	}

	// The id is gone for good: the same repo+generation now mints a
	// fresh lane rather than resurrecting the deleted one.
	if err := release(t, store, "lease-a-again"); err != nil {
		t.Fatal(err)
	}
	fresh, ok, err := m.Acquire(t.Context(), repoURL, "gen", "lease-c", 2)
	if err != nil || !ok {
		t.Fatalf("acquire after delete: ok=%v, %v", ok, err)
	}
	if fresh.LaneID == a.LaneID {
		t.Errorf("a deleted lane id was handed out again: %q", fresh.LaneID)
	}
}

// TestDeleteLaneRefusesALeasedLane: eviction must not race the job that
// is writing to the volume.
func TestDeleteLaneRefusesALeasedLane(t *testing.T) {
	m, _, vols := newManager(t)

	loc, _, err := m.Acquire(t.Context(), repoURL, "gen", "holder", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteLane(t.Context(), loc.LaneID); !errors.Is(err, store.ErrLaneBusy) {
		t.Fatalf("deleting a leased lane = %v; want ErrLaneBusy", err)
	}
	if len(vols.removed) != 0 {
		t.Errorf("a leased lane's volume was removed: %v", vols.removed)
	}
}

// TestDeleteLaneStopsAtAForeignVolume: a name collision must not delete
// someone else's data; ownership is proven before removal.
func TestDeleteLaneStopsAtAForeignVolume(t *testing.T) {
	m, store, vols := newManager(t)

	loc, _, err := m.Acquire(t.Context(), repoURL, "gen", "holder", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(t, store, "holder"); err != nil {
		t.Fatal(err)
	}

	vols.foreign = true
	if err := m.DeleteLane(t.Context(), loc.LaneID); !errors.Is(err, docker.ErrForeignResource) {
		t.Fatalf("deleting over a foreign volume = %v; want ErrForeignResource", err)
	}
	if len(vols.removed) != 0 {
		t.Errorf("a foreign volume was removed: %v", vols.removed)
	}
}
