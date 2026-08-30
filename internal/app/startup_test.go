package app

import (
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"
)

// TestReconcileAdoptsWhatIsStillRunning. A restart finds leases the
// crashed process left behind, and the whole question is which of them
// are still doing work. One with a running capsule is adopted and awaited
// — its job is alive and killing it would lose the run. One whose capsule
// is gone is resolved and released, because nothing will finish it.
//
// Adoption also claims the lease before its goroutine exists: a periodic
// pass that saw it ownerless in that window would tear down a capsule
// that is still running.
func TestReconcileAdoptsWhatIsStillRunning(t *testing.T) {
	h := newHarness(t, 2)
	if err := h.deliver(demand("job-alive", "p1", 1), demand("job-dead", "p1", 2)); err != nil {
		t.Fatal(err)
	}
	alive, _ := leaseFor(t, h, "job-alive")
	dead, _ := leaseFor(t, h, "job-dead")
	for _, l := range []store.Lease{alive, dead} {
		h.inStore(func(tx *store.Tx) error {
			return tx.TransitionLease(l.ID, store.LeaseReserved, store.LeaseProvisioning)
		})
	}

	h.objects.containers = []engine.OwnedContainer{
		{ID: "runner-alive", Role: engine.RoleCapsule, LeaseID: alive.ID, Running: true},
	}
	h.srv.executor.capsule = &fakeCapsule{obs: assignment.ObservedAbsent}
	h.srv.executor.waiter = &fakeWaiter{}

	if err := h.srv.reconciler.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-h.srv.ownership.wait()

	// The adopted lease was awaited to its exit and then released; the
	// one with no capsule was resolved without ever being awaited.
	if got := reloadLease(t, h, alive.ID); got.State != store.LeaseReleased {
		t.Errorf("the adopted lease is %s; want released once its capsule exited", got.State)
	}
	if got := reloadLease(t, h, dead.ID); !got.State.Terminal() {
		t.Errorf("a lease with no capsule left is %s; nothing will ever finish it", got.State)
	}
	// Its container is not swept: adoption is what exempts it, and
	// sweeping a running capsule's objects is the failure that matters.
	for _, id := range h.objects.removed {
		if id == "runner-alive" {
			t.Error("the sweep removed the container of a capsule it had just adopted")
		}
	}
}

// TestReconcileFailsWhenTheDaemonCannotBeRead: startup that cannot see
// the daemon has proven nothing about what is running, and continuing
// would mean deciding every lease's fate against an inventory that was
// never read.
func TestReconcileFailsWhenTheDaemonCannotBeRead(t *testing.T) {
	h := newHarness(t, 1)
	h.objects.listErr = errDaemon

	if err := h.srv.reconciler.reconcile(t.Context()); err == nil {
		t.Error("reconciliation succeeded without reading the daemon")
	}
}

// TestSweepPeriodicallyKeepsWhatIsBeingWorkedOn. The periodic sweep runs
// beside live work, so its keep set is the live leases plus the ones a
// goroutine currently owns. A lease in flight whose objects were swept
// would have its capsule destroyed underneath it.
func TestSweepPeriodicallyKeepsWhatIsBeingWorkedOn(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-1", "p1", 1)); err != nil {
		t.Fatal(err)
	}
	live, _ := leaseFor(t, h, "job-1")
	h.objects.containers = []engine.OwnedContainer{
		{ID: "runner-live", Role: engine.RoleCapsule, LeaseID: live.ID, Running: true},
		{ID: "runner-orphan", Role: engine.RoleCapsule, LeaseID: "lse-vanished"},
	}

	h.srv.reconciler.sweepPeriodically(t.Context())

	for _, id := range h.objects.removed {
		if id == "runner-live" {
			t.Fatal("the periodic sweep removed a live lease's capsule")
		}
	}
	if len(h.objects.removed) != 1 || h.objects.removed[0] != "runner-orphan" {
		t.Errorf("swept %v; want the object of the lease that no longer exists", h.objects.removed)
	}
}
