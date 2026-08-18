package allocator

import "testing"

// TestCapacitySurvivesUnresolvedLease is the allocator's half of the
// capacity rule. The scheduler releases capacity only when its lease
// reaches a terminal state; a lease that ends quarantined still owns privileged
// containers, networks and volumes, so its capacity must stay reserved. This
// asserts the property the allocator has to provide for that to work:
// reservations are held until an explicit release, and a release that
// never comes is not silently forgiven.
func TestCapacitySurvivesUnresolvedLease(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 2)
	mustRegister(t, a, "std", "b", 2)

	// Two capsules run; one ends cleanly, the other is quarantined and so
	// is never released.
	if !a.TryReserve("a") || !a.TryReserve("b") {
		t.Fatal("both capacity units should be claimable")
	}
	a.Release("a") // resolved to released
	// "b" is quarantined: no Release call.

	if got := a.Active("b"); got != 1 {
		t.Fatalf("quarantined lease holds %d capacity units; want 1", got)
	}
	if !a.TryReserve("a") {
		t.Fatal("the freed capacity should be reusable")
	}
	if a.TryReserve("a") {
		t.Error("the quarantined lease's capacity was handed out; the host would be oversubscribed")
	}

	// Once cleanup resolves the quarantine, the capacity returns.
	a.Release("b")
	if !a.TryReserve("b") {
		t.Error("after the quarantine is resolved its capacity should be available again")
	}
}

// TestAdoptAccountsExistingCapsules covers reconciliation: capsules found
// at startup already occupy the host, so adoption must not be gated on
// free budget, and it must still count against it.
func TestAdoptAccountsExistingCapsules(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 1)

	a.Adopt("a") // a capsule survived the previous controller
	if got := a.Active("a"); got != 1 {
		t.Fatalf("adopted capsule not accounted: active = %d", got)
	}
	if a.TryReserve("a") {
		t.Error("the pool is full with the adopted capsule; a new one must be refused")
	}
}
