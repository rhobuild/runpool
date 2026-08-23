package app

import (
	"errors"
	"slices"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
)

// TestSweepOrphansSparesWhatIsNotGarbage drives the sweep at its own call
// site, which is where its mistakes are destructive. Four rules meet
// here, and each one protects something different from being deleted:
// an adopted lease's objects are in use, a helper still measuring is the
// instance's own, persistent infrastructure carries no lease on purpose,
// and everything else is what a sweep exists to collect.
func TestSweepOrphansSparesWhatIsNotGarbage(t *testing.T) {
	h := newHarness(t, 1)
	h.objects.containers = []engine.OwnedContainer{
		{ID: "runner-live", Role: "capsule", LeaseID: "lse-adopted", Running: true},
		{ID: "runner-done", Role: "capsule", LeaseID: "lse-gone"},
		{ID: "probe-running", Role: "probe", Running: true},
		{ID: "probe-leaked", Role: "probe"},
	}
	h.objects.networks = []engine.OwnedResource{
		{ID: "uplink", Role: "uplink"},
		{ID: "net-gone", Role: "capsule-net", LeaseID: "lse-gone"},
	}
	h.objects.volumes = []engine.OwnedResource{
		{ID: "lane", Role: "cache-lane"},
		{ID: "vol-gone", Role: "dind-data", LeaseID: "lse-gone"},
	}

	if err := h.srv.sweepOrphans(t.Context(), func() (map[assignment.LeaseID]bool, error) {
		return map[assignment.LeaseID]bool{"lse-adopted": true}, nil
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{"runner-done", "probe-leaked", "net-gone", "vol-gone"}
	if !slices.Equal(h.objects.removed, want) {
		t.Errorf("swept %v;\nwant only lease garbage and a leaked helper, in dependency order: %v",
			h.objects.removed, want)
	}
}

// TestSweepOrphansSurvivesAnObjectThatWillNotDie: one wedged object used
// to abort startup with no retry, while the per-lease intent saga books
// backoff for the very same work. The pass now counts it and keeps
// going, so the objects behind it are still collected and the next sweep
// finds the survivor by its labels.
func TestSweepOrphansSurvivesAnObjectThatWillNotDie(t *testing.T) {
	h := newHarness(t, 1)
	h.objects.containers = []engine.OwnedContainer{{ID: "wedged", Role: "capsule", LeaseID: "lse-gone"}}
	h.objects.networks = []engine.OwnedResource{{ID: "net-gone", Role: "capsule-net", LeaseID: "lse-gone"}}
	h.objects.volumes = []engine.OwnedResource{{ID: "vol-gone", Role: "dind-data", LeaseID: "lse-gone"}}
	h.objects.wedged["wedged"] = true

	if err := h.srv.sweepOrphans(t.Context(), func() (map[assignment.LeaseID]bool, error) { return nil, nil }); err != nil {
		t.Fatalf("one object that would not die failed the whole pass: %v", err)
	}
	if !slices.Equal(h.objects.removed, []string{"net-gone", "vol-gone"}) {
		t.Errorf("swept %v; the limbs after the wedged object must still run", h.objects.removed)
	}
}

// TestSweepOrphansFailsOnAnUnreadableInventory: a sweep that cannot see
// the daemon has proven nothing, and reporting an empty inventory as a
// clean one is how it would delete nothing and claim success.
func TestSweepOrphansFailsOnAnUnreadableInventory(t *testing.T) {
	h := newHarness(t, 1)
	h.objects.listErr = errors.New("daemon unreachable")

	if err := h.srv.sweepOrphans(t.Context(), func() (map[assignment.LeaseID]bool, error) { return nil, nil }); err == nil {
		t.Error("an unreadable inventory was reported as nothing to sweep")
	}
}

// TestALeaseCommittedDuringTheSweepKeepsItsObjects: the daemon is
// enumerated before the live set is read.
//
// An object exists only after the lease that owns it committed, so an
// object seen now whose lease is absent in the later read is genuinely
// ownerless. Read the other way round, a lease that commits between the
// two reads owns a container the sweep cannot account for, and the sweep
// force-removes a capsule whose job is running, then deletes the records
// that would have cleaned up after it.
//
// The commit is modelled where it actually falls: the fake daemon
// registers the lease as it answers, so the lease exists only for a
// reader that runs after the enumeration.
func TestALeaseCommittedDuringTheSweepKeepsItsObjects(t *testing.T) {
	h := newHarness(t, 1)
	h.objects.containers = []engine.OwnedContainer{
		{ID: "runner-committing", Role: "capsule", LeaseID: "lse-committing", Running: true},
	}
	h.objects.networks = []engine.OwnedResource{
		{ID: "net-committing", Role: "capsule-net", LeaseID: "lse-committing"},
	}
	h.objects.volumes = []engine.OwnedResource{
		{ID: "vol-committing", Role: "dind-data", LeaseID: "lse-committing"},
	}

	// The lease is visible only once every kind has been listed, so a
	// read that runs after the containers but before the volumes is as
	// much a failure as one that runs first: an object listed after the
	// read is an object the read could not account for, whichever kind
	// it is.
	listed := 0
	h.objects.onList = func() { listed++ }

	if err := h.srv.sweepOrphans(t.Context(), func() (map[assignment.LeaseID]bool, error) {
		if listed < 3 {
			return nil, nil
		}
		return map[assignment.LeaseID]bool{"lse-committing": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(h.objects.removed) != 0 {
		t.Errorf("swept %v; the lease committed before every kind had been listed, so the sweep "+
			"tore down a capsule whose job is running and deleted the records that clean up after it",
			h.objects.removed)
	}
}
