package app

import (
	"testing"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// TestCreditsRebuildFromAdoptedCapsules: after a restart the allocator
// starts empty, and reconciliation is what puts the credits back. A
// capsule found running holds its credit again, so the pool does not
// advertise capacity the host is already using.
func TestCreditsRebuildFromAdoptedCapsules(t *testing.T) {
	a := allocator.New()
	for _, key := range []string{"a", "b"} {
		if err := a.Register("std", assignment.BindingKey(key), 2); err != nil {
			t.Fatal(err)
		}
	}
	// Fresh process: nothing is known yet, so both credits look free.
	if got := sumAdvertised(a); got > 2 {
		t.Fatalf("sum(advertised) = %d before reconciliation; want at most 2", got)
	}

	// Reconciliation finds two capsules still running for a.
	a.Adopt("a")
	a.Adopt("a")

	if got := a.Advertised("a"); got != 2 {
		t.Errorf("a advertised %d; want the 2 capsules it is running", got)
	}
	if got := a.Advertised("b"); got != 0 {
		t.Errorf("b advertised %d; the pool's credits are all held", got)
	}
	if got := sumAdvertised(a); got != 2 {
		t.Errorf("sum(advertised) = %d; want exactly the 2 capacity units", got)
	}
	if a.TryReserve("b") {
		t.Error("b took a credit the host does not have")
	}
}

// TestPressureWithholdsCredit: the disk monitor's verdict reaches the
// broker through the allocator. An emergency stops new capacity being
// announced; recovery restores it.
func TestPressureWithholdsCredit(t *testing.T) {
	h := newHarness(t, 2)
	h.srv.alloc.SetAssignedDemand(h.bind.key, 2)

	if got := h.srv.alloc.Advertised(h.bind.key); got == 0 {
		t.Fatal("a binding with demand and free capacity must advertise")
	}

	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.SetPressure(store.PressureInfo{Level: disk.SoftEmergency.String()})
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.disk.resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.alloc.Advertised(h.bind.key); got != 0 {
		t.Errorf("advertised %d under a soft emergency; want 0 new capacity", got)
	}

	h.srv.alloc.Hold(false)
	if got := h.srv.alloc.Advertised(h.bind.key); got != 2 {
		t.Errorf("advertised %d after recovery; want the demand back", got)
	}
}

// TestSilentBindingIsFoundByRotation is the trapdoor at the level that
// matters: several bindings share one tier, none has demand, and the
// rotation must reach each of them — otherwise the one with a queued
// job never learns it exists.
func TestSilentBindingIsFoundByRotation(t *testing.T) {
	a := allocator.New()
	keys := []string{"repo-a", "repo-b", "repo-c", "repo-d"}
	for _, k := range keys {
		if err := a.Register("std", assignment.BindingKey(k), 2); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range len(keys) {
		for _, k := range keys {
			if a.Advertised(assignment.BindingKey(k)) > 0 {
				seen[k] = true
			}
		}
		if got := sumAdvertised(a); got > 2 {
			t.Fatalf("sum(advertised) = %d during rotation; want at most 2", got)
		}
		a.Rotate()
	}
	for _, k := range keys {
		if !seen[k] {
			t.Errorf("binding %q was never offered the discovery credit; it would stay blind", k)
		}
	}
}

func sumAdvertised(a *allocator.Allocator) int {
	total := 0
	for _, v := range a.AdvertisedAll("std") {
		total += v
	}
	return total
}

// TestLaneCeilingFollowsTheTighterLimit: cache lanes are sized off how many
// leases can actually run at once. A tier that could hold four is held to one
// by an instance-wide limit, and provisioning four lanes for it would reserve
// disk for leases that can never exist.
func TestLaneCeilingFollowsTheTighterLimit(t *testing.T) {
	tier := config.Tier{ID: "std", Parallelism: 4}

	independent := &config.Config{}
	if got := laneCeiling(independent, tier); got != 4 {
		t.Errorf("lane ceiling = %d with independent tiers; want the tier's own 4", got)
	}

	one := 1
	global := &config.Config{Scheduling: config.Scheduling{Parallelism: &one}}
	if got := laneCeiling(global, tier); got != 1 {
		t.Errorf("lane ceiling = %d under an instance limit of one; want 1", got)
	}

	// A global limit looser than the tier does not raise the tier's own cap.
	ten := 10
	loose := &config.Config{Scheduling: config.Scheduling{Parallelism: &ten}}
	if got := laneCeiling(loose, tier); got != 4 {
		t.Errorf("lane ceiling = %d under a looser instance limit; want the tier's 4", got)
	}
}
