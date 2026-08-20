package app

import (
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// Permanent regressions for the durable assignment machine. Each names
// the window it covers, because each is a way the machine loses or
// duplicates work that no unit of SQLite behaviour on its own reveals.

// TestLateRedeliveryDoesNotRecreateWork covers the window between a
// failed acknowledgement and the broker's redelivery. The job ran to
// completion; when the same message arrives again, the settled attempt
// under the same delivery is what recognises it, and no second capsule
// starts.
func TestLateRedeliveryDoesNotRecreateWork(t *testing.T) {
	h := newHarness(t, 1)
	probe := demand("probe-job", "app", 42)

	if err := h.deliverMsg(42, probe); err != nil {
		t.Fatal(err)
	}
	// The acknowledgement fails here; the broker will send it again.
	lease, attemptID := leaseFor(t, h, "probe-job")
	driveLeaseTo(t, h, lease.ID, store.LeaseCleaning)
	h.recordEvidence(lease.ID, store.EvidenceExitObserved)
	if err := h.srv.leases.Finalize(t.Context(), lease.ID, ""); err != nil {
		t.Fatal(err)
	}

	// The redelivery arrives after the job has finished.
	if err := h.deliverMsg(42, probe); err != nil {
		t.Fatal(err)
	}
	if got := h.ready(); len(got) != 0 {
		t.Errorf("a completed job was queued again by a late redelivery: %+v", got)
	}
	if got := attemptState(t, h, attemptID); got.State != store.AttemptSettled || got.Resolution != assignment.ResolutionCompletedObserved {
		t.Errorf("attempt = %s/%s; want settled/completed_observed", got.State, got.Resolution)
	}
}

// TestInterruptedLeaseReturnsItsAttempt covers a crash after the lease
// exists but before the runner ran. Startup releases the lease, but if
// the attempt stayed leased to it, the work is neither running nor
// queued: it is invisible, and GitHub will not send it again.
func TestInterruptedLeaseReturnsItsAttempt(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("probe-restart", "app", 43)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "probe-restart")

	// Startup finds a reserved lease with no runner.
	h.resolveWithoutRuntime(t.Context(), lease)

	if got := h.ready(); len(got) != 1 {
		t.Fatalf("the attempt did not return to the queue: %+v", got)
	}
}

// TestFailedAndQuarantinedLeasesReachReleased covers recovery from the
// two states that were once sent through a transition they cannot make:
// failed -> failed and quarantined -> failed are both invalid, so
// cleanup returned before touching anything.
func TestFailedAndQuarantinedLeasesReachReleased(t *testing.T) {
	for _, from := range []store.LeaseState{store.LeaseFailed, store.LeaseQuarantined} {
		t.Run(string(from), func(t *testing.T) {
			h := newHarness(t, 1)
			if err := h.deliver(demand("probe-"+string(from), "app", 44)); err != nil {
				t.Fatal(err)
			}
			lease, _ := leaseFor(t, h, assignment.SourceWorkloadKey("probe-"+string(from)))
			driveLeaseTo(t, h, lease.ID, from)

			h.resolveWithoutRuntime(t.Context(), lease)

			final := reloadLease(t, h, lease.ID)
			if final.State != store.LeaseReleased {
				t.Errorf("a %s lease stayed %s; recovery never ran its cleanup", from, final.State)
			}
		})
	}
}

func reloadLease(t *testing.T, h *harness, leaseID assignment.LeaseID) store.Lease {
	t.Helper()
	var lease store.Lease
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		lease, err = tx.LeaseByID(leaseID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lease
}
