package app

import (
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// Startup recovery tests execute the real reconciler against durable leases
// in SQLite and assert the resulting lease and attempt states.

// reachableEvidence lists, per capsule state, the evidence values a
// crash can actually leave together with it. Preparation and start
// authorization happen while the lease is runtime_registered, and the
// running/exit observations from workload_running onward; failure states
// are reachable from any point, so they combine with everything.
func reachableEvidence(s store.LeaseState) []store.Evidence {
	all := []store.Evidence{
		store.EvidenceNotStarted, store.EvidenceRuntimePrepared,
		store.EvidenceStartAuthorized, store.EvidenceRunningObserved,
		store.EvidenceExitObserved,
	}
	switch s {
	case store.LeaseReserved, store.LeaseProvisioning:
		return all[:1] // nothing is prepared before registration
	case store.LeaseWorkloadRunning:
		// The transition to workload_running happens once a start
		// succeeded; authorized-only is possible when recording the
		// running observation failed and the loop continued.
		return all[2:]
	default:
		return all
	}
}

// TestRecoveryMatrix is the cartesian product of capsule states and the
// evidence reachable with them. Two properties hold across the whole
// matrix, and they are deliberately independent:
//
//   - cleanup: every lease ends released, whatever state the crash left,
//     because a lease that cannot be resolved holds its credit forever;
//   - disposition: what becomes of the attempt follows the evidence
//     alone — requeued when nothing was authorized, settled with the
//     observation as its resolution when execution was seen, held for
//     review when a start was authorized and no runtime remains to
//     prove its outcome.
func TestRecoveryMatrix(t *testing.T) {
	states := []store.LeaseState{
		store.LeaseReserved, store.LeaseProvisioning, store.LeaseRuntimeRegistered,
		store.LeaseWorkloadRunning, store.LeaseDraining, store.LeaseCleaning,
		store.LeaseFailed, store.LeaseQuarantined, store.LeaseReleased,
	}
	for _, from := range states {
		for _, evidence := range reachableEvidence(from) {
			t.Run(string(from)+"/"+string(evidence), func(t *testing.T) {
				h := newHarness(t, 1)
				workload := "job-" + string(from) + "-" + string(evidence)
				if err := h.deliver(demand(workload, "app", 2)); err != nil {
					t.Fatal(err)
				}
				lease, attemptID := leaseFor(t, h, assignment.SourceWorkloadKey(workload))
				driveLeaseTo(t, h, lease.ID, from)
				h.recordEvidence(lease.ID, evidence)

				h.resolveWithoutRuntime(t.Context(), reloadLease(t, h, lease.ID))

				if got := reloadLease(t, h, lease.ID); got.State != store.LeaseReleased {
					t.Errorf("a %s lease ended %s; it still holds its credit", from, got.State)
				}

				var (
					wantState      store.AttemptState
					wantResolution assignment.Resolution
				)
				switch evidence {
				case store.EvidenceNotStarted, store.EvidenceRuntimePrepared:
					wantState = store.AttemptReady
				case store.EvidenceStartAuthorized:
					// No runtime remains; nothing can be proven.
					wantState = store.AttemptManualReview
				case store.EvidenceRunningObserved:
					wantState, wantResolution = store.AttemptSettled, assignment.ResolutionStartedObserved
				default:
					wantState, wantResolution = store.AttemptSettled, assignment.ResolutionCompletedObserved
				}
				got := attemptState(t, h, attemptID)
				if got.State != wantState {
					t.Errorf("attempt = %s from %s with %s; want %s", got.State, from, evidence, wantState)
				}
				if wantResolution != "" && got.Resolution != wantResolution {
					t.Errorf("resolution = %s from %s with %s; want %s", got.Resolution, from, evidence, wantResolution)
				}
			})
		}
	}
}

// TestCapsuleFailureRecoverySurvivesAMissingBinding: a lease whose
// target is no longer configured reaches recoverCapsuleFailure with a
// nil binding, which must not panic — it happens during startup, where
// a panic takes down recovery for every other lease with it.
func TestCapsuleFailureRecoverySurvivesAMissingBinding(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-orphan", "app", 3)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-orphan")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("recoverCapsuleFailure panicked with a missing binding: %v", r)
		}
	}()
	if err := h.srv.recoverCapsuleFailure(t.Context(), nil, lease.ID, assignment.NoObservation); err != nil {
		t.Fatalf("recoverCapsuleFailure with a missing binding: %v", err)
	}
}

// TestAdoptableStopsAtDraining: adoption means "still executing, wait it
// out". Past draining the lease is being unwound, and adopting one would
// walk it to running and then attempt a release guarded on
// workload_running — a state conflict that returns before any cleanup,
// leaving the credit held and the privileged container exempt from the
// orphan sweep for the life of the process.
func TestAdoptableStopsAtDraining(t *testing.T) {
	for state, want := range map[store.LeaseState]bool{
		store.LeaseReserved:          true,
		store.LeaseProvisioning:      true,
		store.LeaseRuntimeRegistered: true,
		store.LeaseWorkloadRunning:   true,
		store.LeaseDraining:          false,
		store.LeaseCleaning:          false,
		store.LeaseFailed:            false,
		store.LeaseQuarantined:       false,
		store.LeaseReleased:          false,
	} {
		if got := adoptable(state); got != want {
			t.Errorf("adoptable(%s) = %v; want %v", state, got, want)
		}
	}
}

// TestPrunePeriodicallyHonoursTheWindow covers the wiring, not the
// predicate: the SQL is proved in internal/store, where a lease can be
// aged. What only this layer can show is that the window reaches the
// pass at all — zero means the operator asked for every record to be
// kept and nothing runs, and a live window does not touch work that just
// finished.
func TestPrunePeriodicallyHonoursTheWindow(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-1", "p1", 1)); err != nil {
		t.Fatal(err)
	}
	h.serve()
	launched := h.launchedAttempts()
	if len(launched) != 1 {
		t.Fatalf("launched %v; want one", launched)
	}
	lease := h.leases[launched[0]]
	h.inStore(func(tx *store.Tx) error {
		for _, step := range [][2]store.LeaseState{
			{store.LeaseReserved, store.LeaseProvisioning},
			{store.LeaseProvisioning, store.LeaseRuntimeRegistered},
			{store.LeaseRuntimeRegistered, store.LeaseDraining},
			{store.LeaseDraining, store.LeaseCleaning},
			{store.LeaseCleaning, store.LeaseReleased},
		} {
			if err := tx.TransitionLease(lease.ID, step[0], step[1]); err != nil {
				return err
			}
		}
		return tx.Settle(lease.AttemptID, store.AttemptLeased, "completed_observed")
	})

	for _, window := range []time.Duration{0, 24 * time.Hour} {
		h.srv.leaseHistory = window
		h.srv.prunePeriodically(h.t.Context())
		h.inStore(func(tx *store.Tx) error {
			if _, err := tx.LeaseByID(lease.ID); err != nil {
				t.Errorf("a %s window forgot a lease that finished a moment ago: %v", window, err)
			}
			return nil
		})
	}
}

// TestRecoveryRefinesFromTheRuntimeItStillHolds is the half of the
// matrix above that a crash with a surviving container reaches: the
// authorized start whose outcome the container itself can answer. The
// matrix hands recovery no runtime, so its start_authorized row is held
// for a person — right when nothing remains to ask, and the only
// answer when something does. An exited runtime settles the attempt as
// completed; one the daemon proves never started returns it to the
// queue. Both otherwise land in manual review, which is a person paged
// for a question the pass could have answered itself.
func TestRecoveryRefinesFromTheRuntimeItStillHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		obs            assignment.ExecutionObservation
		wantState      store.AttemptState
		wantResolution assignment.Resolution
	}{
		"an exited runtime settles as completed": {
			obs: assignment.ObservedExited, wantState: store.AttemptSettled,
			wantResolution: assignment.ResolutionCompletedObserved,
		},
		"a runtime that never started returns the attempt to the queue": {
			obs: assignment.ObservedCreated, wantState: store.AttemptReady,
		},
		"a runtime that cannot answer holds the attempt for a person": {
			obs: assignment.ObservedUnavailable, wantState: store.AttemptManualReview,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 1)
			workload := "job-refine-" + string(tc.obs)
			if err := h.deliver(demand(workload, "app", 4)); err != nil {
				t.Fatal(err)
			}
			lease, attemptID := leaseFor(t, h, assignment.SourceWorkloadKey(workload))
			driveLeaseTo(t, h, lease.ID, store.LeaseWorkloadRunning)
			h.recordEvidence(lease.ID, store.EvidenceStartAuthorized)

			h.resolveWithRuntime(t.Context(), reloadLease(t, h, lease.ID), tc.obs)

			if got := reloadLease(t, h, lease.ID); got.State != store.LeaseReleased {
				t.Errorf("lease = %s; want released", got.State)
			}
			got := attemptState(t, h, attemptID)
			if got.State != tc.wantState {
				t.Errorf("attempt = %s with observation %s; want %s", got.State, tc.obs, tc.wantState)
			}
			if got.Resolution != tc.wantResolution {
				t.Errorf("resolution = %q; want %q", got.Resolution, tc.wantResolution)
			}
		})
	}
}
