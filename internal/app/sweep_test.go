package app

import (
	"errors"
	"github.com/rhobuild/runpool/internal/assignment"
	"slices"
	"testing"

	"github.com/rhobuild/runpool/internal/platform/docker"
)

// TestSweepOrphansSparesWhatIsNotGarbage drives the sweep at its own call
// site, which is where its mistakes are destructive. Four rules meet
// here, and each one protects something different from being deleted:
// an adopted lease's objects are in use, a helper still measuring is the
// instance's own, persistent infrastructure carries no lease on purpose,
// and everything else is what a sweep exists to collect.
func TestSweepOrphansSparesWhatIsNotGarbage(t *testing.T) {
	h := newHarness(t, 1)
	h.objects.containers = []docker.OwnedContainer{
		{ID: "runner-live", Role: "capsule", LeaseID: "lse-adopted", Running: true},
		{ID: "runner-done", Role: "capsule", LeaseID: "lse-gone"},
		{ID: "probe-running", Role: "probe", Running: true},
		{ID: "probe-leaked", Role: "probe"},
	}
	h.objects.networks = []docker.OwnedResource{
		{ID: "uplink", Role: "uplink"},
		{ID: "net-gone", Role: "capsule-net", LeaseID: "lse-gone"},
	}
	h.objects.volumes = []docker.OwnedResource{
		{ID: "lane", Role: "cache-lane"},
		{ID: "vol-gone", Role: "dind-data", LeaseID: "lse-gone"},
	}

	if err := h.srv.sweepOrphans(t.Context(), map[assignment.LeaseID]bool{"lse-adopted": true}); err != nil {
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
	h.objects.containers = []docker.OwnedContainer{{ID: "wedged", Role: "capsule", LeaseID: "lse-gone"}}
	h.objects.networks = []docker.OwnedResource{{ID: "net-gone", Role: "capsule-net", LeaseID: "lse-gone"}}
	h.objects.volumes = []docker.OwnedResource{{ID: "vol-gone", Role: "dind-data", LeaseID: "lse-gone"}}
	h.objects.wedged["wedged"] = true

	if err := h.srv.sweepOrphans(t.Context(), nil); err != nil {
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

	if err := h.srv.sweepOrphans(t.Context(), nil); err == nil {
		t.Error("an unreadable inventory was reported as nothing to sweep")
	}
}
