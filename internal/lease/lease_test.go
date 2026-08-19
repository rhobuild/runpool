package lease

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// nopRemover stands in for the daemon on the removal side: absence is
// success, exactly the contract the real client honours.
type nopRemover struct{}

func (nopRemover) RemoveOwnedContainer(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return nil
}
func (nopRemover) RemoveOwnedNetwork(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return nil
}
func (nopRemover) RemoveOwnedVolume(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return nil
}

// wedgedRemover fails every removal until healed — the daemon a
// quarantined lease is waiting on.
type wedgedRemover struct{ healed bool }

func (w *wedgedRemover) fail() error {
	if w.healed {
		return nil
	}
	return errors.New("daemon wedged")
}
func (w *wedgedRemover) RemoveOwnedContainer(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return w.fail()
}
func (w *wedgedRemover) RemoveOwnedNetwork(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return w.fail()
}
func (w *wedgedRemover) RemoveOwnedVolume(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return w.fail()
}

type fixture struct {
	t       *testing.T
	m       *Manager
	store   *store.Store
	lease   store.Lease
	attempt string
}

// newFixture builds one binding, one delivered workload and the lease
// serving it — the state every lease-machine test starts from.
func newFixture(t *testing.T, remove Remover) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, store: st,
		m: NewManager(st, remove, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	f.tx(func(tx *store.Tx) error {
		binding, err := tx.EnsureBinding("app", "github_actions",
			"v1|repository|https://github.com/acme/app||runpool-standard")
		if err != nil {
			return err
		}
		if _, err := tx.RecordDelivery(binding, "msg-1", sha256.Sum256([]byte("payload")),
			[]store.WorkloadRow{{SourceWorkloadKey: "job-1", TenantKey: "acme", ProjectKey: "app"}}); err != nil {
			return err
		}
		ready, err := tx.ReadyAttempts(binding)
		if err != nil {
			return err
		}
		f.attempt = string(ready[0].ID)
		f.lease, err = tx.LeaseAttempt(assignment.AttemptID(f.attempt), binding, "standard")
		return err
	})
	return f
}

func (f *fixture) tx(fn func(*store.Tx) error) {
	f.t.Helper()
	if err := f.store.Tx(f.t.Context(), fn); err != nil {
		f.t.Fatal(err)
	}
}

// driveTo walks the lease to a target state through the real machine, so
// a test never fabricates a state the product cannot produce.
func (f *fixture) driveTo(target store.LeaseState) {
	f.t.Helper()
	paths := map[store.LeaseState][]store.LeaseState{
		store.LeaseReserved:          {},
		store.LeaseProvisioning:      {store.LeaseProvisioning},
		store.LeaseRuntimeRegistered: {store.LeaseProvisioning, store.LeaseRuntimeRegistered},
		store.LeaseWorkloadRunning: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning},
		store.LeaseDraining: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining},
		store.LeaseCleaning: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining, store.LeaseCleaning},
		store.LeaseQuarantined: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining, store.LeaseCleaning,
			store.LeaseQuarantined},
	}
	path, ok := paths[target]
	if !ok {
		f.t.Fatalf("no path defined to lease state %q", target)
	}
	f.tx(func(tx *store.Tx) error {
		from := store.LeaseReserved
		for _, to := range path {
			if err := tx.TransitionLease(f.lease.ID, from, to); err != nil {
				return err
			}
			from = to
		}
		return nil
	})
	if got := f.reload(); got.State != target {
		f.t.Fatalf("wanted a lease in %q but it is %q; the test would assert nothing", target, got.State)
	}
}

func (f *fixture) reload() store.Lease {
	f.t.Helper()
	var lease store.Lease
	f.tx(func(tx *store.Tx) error {
		var err error
		lease, err = tx.LeaseByID(f.lease.ID)
		return err
	})
	return lease
}

func (f *fixture) attemptState() store.Attempt {
	f.t.Helper()
	var a store.Attempt
	f.tx(func(tx *store.Tx) error {
		var err error
		a, err = tx.Get(assignment.AttemptID(f.attempt))
		return err
	})
	return a
}

// advanceAttemptTo walks the attempt through the real transitions the
// serving path uses, so a test never writes a state the product cannot
// reach.
func (f *fixture) advanceAttemptTo(target string) {
	f.t.Helper()
	ladder := []string{"leased", "preparing", "prepared", "starting", "running"}
	f.tx(func(tx *store.Tx) error {
		for i := 1; i < len(ladder); i++ {
			if err := tx.Advance(assignment.AttemptID(f.attempt), ladder[i-1], ladder[i]); err != nil {
				return err
			}
			if ladder[i] == target {
				return nil
			}
		}
		return fmt.Errorf("no path defined to attempt state %q", target)
	})
	if got := f.attemptState().State; got != target {
		f.t.Fatalf("wanted an attempt in %q but it is %q; the test would assert nothing", target, got)
	}
}

func (f *fixture) recordEvidence(e store.Evidence) {
	f.t.Helper()
	if err := f.m.RecordEvidence(f.t.Context(), f.lease.ID, e); err != nil {
		f.t.Fatal(err)
	}
}

// Finalize commits release, cache-lane return and attempt disposition in
// one transaction, deciding by what can be proven about execution. Work
// that provably never began must return to the queue, observed execution
// must settle with the observation as its resolution, and an unprovable
// start outcome must be held for a person — visible, never silently
// either of the others. A lease that is not cleaning refuses to
// finalize, and the refusal leaves the attempt untouched: atomicity
// means all of it or none of it.
func TestFinalizeDisposesByEvidence(t *testing.T) {
	cases := []struct {
		name           string
		evidence       store.Evidence
		toState        store.LeaseState
		startObs       assignment.ExecutionObservation
		wantErr        bool
		wantState      string
		wantResolution string
	}{
		{"exit observed", store.EvidenceExitObserved, store.LeaseCleaning, "", false, "settled", assignment.ResolutionCompletedObserved},
		{"running observed", store.EvidenceRunningObserved, store.LeaseCleaning, "", false, "settled", assignment.ResolutionStartedObserved},
		{"nothing was prepared", store.EvidenceNotStarted, store.LeaseCleaning, "", false, "ready", ""},
		{"prepared but never authorized", store.EvidenceRuntimePrepared, store.LeaseCleaning, "", false, "ready", ""},
		{"authorized and unprovable", store.EvidenceStartAuthorized, store.LeaseCleaning, assignment.ObservedAbsent, false, "manual_review", ""},
		{"authorized and proven inert", store.EvidenceStartAuthorized, store.LeaseCleaning, assignment.ObservedCreated, false, "ready", ""},
		{"lease not yet cleaning", store.EvidenceNotStarted, store.LeaseQuarantined, "", true, "leased", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nopRemover{})
			f.driveTo(tc.toState)
			f.recordEvidence(tc.evidence)

			err := f.m.Finalize(t.Context(), f.lease.ID, tc.startObs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Finalize error = %v; wantErr %v", err, tc.wantErr)
			}

			got := f.attemptState()
			if got.State != tc.wantState {
				t.Errorf("attempt state = %s; want %s", got.State, tc.wantState)
			}
			if tc.wantResolution != "" && got.Resolution != tc.wantResolution {
				t.Errorf("resolution = %s; want %s", got.Resolution, tc.wantResolution)
			}
			if !tc.wantErr && f.reload().State != store.LeaseReleased {
				t.Errorf("lease = %s; want released in the same commit", f.reload().State)
			}
		})
	}
}

// The finalizing transaction is atomic: when any step inside it fails,
// nothing commits — the lease stays cleaning and the attempt stays
// leased, so reconciliation retries the whole ending instead of
// observing half of one. The failure injected here is a resource record
// that external cleanup never removed.
func TestFinalizeIsAtomic(t *testing.T) {
	f := newFixture(t, nopRemover{})
	f.driveTo(store.LeaseCleaning)
	f.recordEvidence(store.EvidenceExitObserved)

	f.tx(func(tx *store.Tx) error {
		id, err := tx.PlanResource(f.lease.ID, store.ResourceContainer, "runner", "runpool-runner-leftover")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(assignment.ResourceIntentID(id), "leftover")
	})

	if err := f.m.Finalize(t.Context(), f.lease.ID, ""); err == nil {
		t.Fatal("finalizing with a surviving resource record succeeded")
	}
	if got := f.reload(); got.State != store.LeaseCleaning {
		t.Errorf("lease = %s; want still cleaning — nothing may commit", got.State)
	}
	if got := f.attemptState(); got.State != "leased" {
		t.Errorf("attempt = %s; want still leased — nothing may commit", got.State)
	}
}

// Release walks a live lease all the way to released, removing every
// owned object on the way and disposing of the attempt by evidence.
func TestReleaseRemovesEverythingAndDisposes(t *testing.T) {
	f := newFixture(t, nopRemover{})
	f.driveTo(store.LeaseWorkloadRunning)
	f.recordEvidence(store.EvidenceExitObserved)
	f.tx(func(tx *store.Tx) error {
		id, err := tx.PlanResource(f.lease.ID, store.ResourceContainer, "runner", "runpool-runner-1")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(assignment.ResourceIntentID(id), "runner-1")
	})

	if err := f.m.Release(t.Context(), f.lease.ID, store.LeaseWorkloadRunning); err != nil {
		t.Fatal(err)
	}
	if got := f.reload(); got.State != store.LeaseReleased {
		t.Errorf("lease = %s; want released", got.State)
	}
	f.tx(func(tx *store.Tx) error {
		intents, err := tx.Resources(f.lease.ID)
		if err != nil {
			return err
		}
		if len(intents) != 0 {
			t.Errorf("intents = %d; every one must be forgotten before release commits", len(intents))
		}
		return nil
	})
	if got := f.attemptState(); got.State != "settled" {
		t.Errorf("attempt = %s; want settled", got.State)
	}
}

// A wedged daemon parks the lease in quarantine with backoff booked on
// the intent, and the attempt stays unresolved — visible, waiting. The
// lease keeps its credit precisely because its privileged containers may
// still exist.
func TestReleaseQuarantinesOnAWedgedDaemon(t *testing.T) {
	remover := &wedgedRemover{}
	f := newFixture(t, remover)
	f.driveTo(store.LeaseWorkloadRunning)
	f.tx(func(tx *store.Tx) error {
		id, err := tx.PlanResource(f.lease.ID, store.ResourceContainer, "runner", "runpool-runner-wedge")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(assignment.ResourceIntentID(id), "wedge-1")
	})

	if err := f.m.Release(t.Context(), f.lease.ID, store.LeaseWorkloadRunning); err == nil {
		t.Fatal("release with a wedged daemon succeeded")
	}
	if got := f.reload(); got.State != store.LeaseQuarantined {
		t.Fatalf("lease = %s; want quarantined", got.State)
	}
	if got := f.attemptState(); got.State != "leased" {
		t.Errorf("attempt = %s; want still leased — nothing was resolved", got.State)
	}

	// The intent carries a booked backoff, so the lease is not due yet.
	due, err := f.m.IntentsDue(t.Context(), f.lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("a lease whose removal just failed is due immediately; the backoff was not booked")
	}

	// The daemon heals and the window elapses.
	remover.healed = true
	f.tx(func(tx *store.Tx) error {
		intents, err := tx.Resources(f.lease.ID)
		if err != nil {
			return err
		}
		return tx.RecordResourceError(intents[0].ID, errors.New("previous failure"), time.Now().Add(-time.Second))
	})
	if due, err := f.m.IntentsDue(t.Context(), f.lease.ID); err != nil || !due {
		t.Fatalf("after the backoff the lease must be due: due=%v, %v", due, err)
	}
	// A quarantined lease resumes through cleaning; moving it to failed
	// again is the invalid transition that once aborted the retry.
	if _, err := f.m.ToCleaning(t.Context(), f.lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.FinishCleaning(t.Context(), f.reload(), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.reload(); got.State != store.LeaseReleased {
		t.Errorf("lease = %s after the daemon healed; want released", got.State)
	}
}

// The intent recorder is what makes a crash recoverable: the plan is
// durable before the create call runs, in its own transaction, so it
// survives the crash it exists for.
func TestRecorderCommitsEachStepSeparately(t *testing.T) {
	f := newFixture(t, nopRemover{})
	rec := f.m.Recorder(t.Context(), f.lease.ID)

	id, err := rec.Plan("container", "runner", "runpool-runner-x")
	if err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx *store.Tx) error {
		intents, err := tx.Resources(f.lease.ID)
		if err != nil {
			return err
		}
		if len(intents) != 1 || intents[0].State != "planned" {
			t.Fatalf("after Plan the store holds %+v; want one planned intent", intents)
		}
		return nil
	})
	if err := rec.Creating(id); err != nil {
		t.Fatal(err)
	}
	if err := rec.Confirm(id, "docker-abc"); err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx *store.Tx) error {
		intents, err := tx.Resources(f.lease.ID)
		if err != nil {
			return err
		}
		if intents[0].State != "present" || intents[0].Handle() != "docker-abc" {
			t.Errorf("confirmed intent = %s/%s; want present, addressed by id",
				intents[0].State, intents[0].Handle())
		}
		return nil
	})
}

// TestTheExhaustedBudgetHoldsTheAttemptForReview covers the manager's half
// of the retry budget: the store refuses the fourth serving's requeue, and
// this is where that refusal has to become a review instead of an error.
// An error here fails Finalize's transaction, which rolls back the lease
// release with it - so the credit is never freed and reconciliation
// retries the same failing commit forever.
func TestTheExhaustedBudgetHoldsTheAttemptForReview(t *testing.T) {
	f := newFixture(t, nopRemover{})

	// Serve and dispose through the real machine until the budget is
	// spent. Each pass is one serving: drive to cleaning, finalize with
	// retriable evidence, and the attempt returns to ready.
	for serving := 1; ; serving++ {
		f.driveTo(store.LeaseCleaning)
		if err := f.m.Finalize(t.Context(), f.lease.ID, ""); err != nil {
			t.Fatalf("finalize serving %d: %v", serving, err)
		}
		var attempt store.Attempt
		f.tx(func(tx *store.Tx) error {
			var err error
			attempt, err = tx.Get(assignment.AttemptID(f.attempt))
			return err
		})
		if attempt.State == "manual_review" {
			if attempt.ReviewReason != store.ReviewReasonRetryBudgetExhausted {
				t.Fatalf("held for review as %q; want %q, so an operator knows the "+
					"question is whether retrying will ever stop",
					attempt.ReviewReason, store.ReviewReasonRetryBudgetExhausted)
			}
			return
		}
		if attempt.State != "ready" {
			t.Fatalf("after serving %d the attempt is %q; want ready or manual_review", serving, attempt.State)
		}
		if serving > 5 {
			t.Fatal("the budget never ended; a failure that repeats is retried without bound")
		}
		// The next serving.
		f.tx(func(tx *store.Tx) error {
			var err error
			f.lease, err = tx.LeaseAttempt(assignment.AttemptID(f.attempt), attempt.BindingID, "standard")
			return err
		})
	}
}

// TestAReservedExitRequeuesAnAttemptTheWalkAlreadyCalledRunning: the
// walk to `running` is optimistic. It records that a start was
// authorized and the capsule reported itself up — never that a runner
// took the job — so a runtime that afterwards proves the start never
// took effect must still return the workload to the queue.
//
// Judged in evidence order instead, this attempt settles as one that
// started, and the workload is never served again. That is the shape of
// a capsule whose inner daemon never came up: everything the controller
// wrote is intention, and the only fact is the reserved exit code.
func TestAReservedExitRequeuesAnAttemptTheWalkAlreadyCalledRunning(t *testing.T) {
	f := newFixture(t, nopRemover{})
	f.driveTo(store.LeaseCleaning)
	f.advanceAttemptTo("running")
	f.recordEvidence(store.EvidenceRunningObserved)

	if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.ObservedCreated); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if got := f.attemptState(); got.State != "ready" {
		t.Errorf("attempt state = %s (resolution %q); want it back in the queue as ready",
			got.State, got.Resolution)
	}
	if got := f.reload().State; got != store.LeaseReleased {
		t.Errorf("lease = %s; want released in the same commit", got)
	}
}

// TestAHeldAttemptDoesNotPinItsLease: a serving that ends against an
// attempt somebody else already resolved still releases its lease.
//
// An attempt held for review is not this lease's to dispose of — a
// person owns it now. Writing a disposition onto it anyway matches no
// row, fails, and rolls the release back with it: the lease stays in
// cleaning holding its admission credit, and because a lease already in
// cleaning cannot move there again, every later reconciliation pass
// repeats the identical rollback and the capacity is gone for good.
func TestAHeldAttemptDoesNotPinItsLease(t *testing.T) {
	f := newFixture(t, nopRemover{})
	f.driveTo(store.LeaseCleaning)
	id := assignment.AttemptID(f.attempt)
	f.tx(func(tx *store.Tx) error {
		return tx.HoldForReview(id, store.ReviewReasonIncompatibleCapsule)
	})

	if err := f.m.Finalize(t.Context(), f.lease.ID, ""); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got := f.reload().State; got != store.LeaseReleased {
		t.Errorf("lease = %s; want released — a held attempt must not pin the lease serving it", got)
	}
	if got := f.attemptState(); got.State != "manual_review" {
		t.Errorf("attempt = %s; want it left in manual_review for the person who holds it", got.State)
	}
}

// TestBothDispositionPathsAgree: the finalizing transaction and the
// stranded-lease sweep decide the same thing about the same attempt.
//
// They once carried a switch each and the cases drifted into different
// orders, so an attempt with a proven-inert start returned to the queue
// through one and settled as a job that ran through the other. The
// decision is one function now; this is what says so.
func TestBothDispositionPathsAgree(t *testing.T) {
	for name, tc := range map[string]struct {
		state    string
		evidence store.Evidence
		obs      assignment.ExecutionObservation
		want     disposition
	}{
		"proven inert outranks a running observation": {
			"running", store.EvidenceRunningObserved, assignment.ObservedCreated, dispositionRequeue},
		"an observed exit settles as completed": {
			"running", store.EvidenceExitObserved, "", dispositionSettleCompleted},
		"a running observation settles as started": {
			"running", store.EvidenceRunningObserved, "", dispositionSettleStarted},
		"nothing prepared returns to the queue": {
			"leased", store.EvidenceNotStarted, "", dispositionRequeue},
		"a running runtime settles as started": {
			"starting", store.EvidenceStartAuthorized, assignment.ObservedRunning, dispositionSettleStarted},
		"an unprovable start is held": {
			"starting", store.EvidenceStartAuthorized, assignment.ObservedAbsent, dispositionReview},
		"a superseded attempt is left alone": {
			"superseded", store.EvidenceNotStarted, "", dispositionNone},
		"a held attempt is left alone": {
			"manual_review", store.EvidenceNotStarted, "", dispositionNone},
		"a settled attempt is left alone": {
			"settled", store.EvidenceExitObserved, "", dispositionNone},
	} {
		t.Run(name, func(t *testing.T) {
			attempt := store.Attempt{State: tc.state, Evidence: tc.evidence}
			if got := dispositionFor(attempt, tc.obs); got != tc.want {
				t.Errorf("dispositionFor(%s, %s, %s) = %d; want %d",
					tc.state, tc.evidence, tc.obs, got, tc.want)
			}
		})
	}
}

// TestAProvenInertStartIsRequeuedFromEitherPath drives the same lease
// state through both endings and requires the same outcome.
func TestAProvenInertStartIsRequeuedFromEitherPath(t *testing.T) {
	for name, end := range map[string]func(*fixture){
		"the finalizing transaction": func(f *fixture) {
			if err := f.m.Finalize(f.t.Context(), f.lease.ID, assignment.ObservedCreated); err != nil {
				f.t.Fatalf("Finalize: %v", err)
			}
		},
		"the stranded sweep": func(f *fixture) {
			f.m.DisposeStranded(f.t.Context(), f.reload(), assignment.ObservedCreated)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nopRemover{})
			f.driveTo(store.LeaseCleaning)
			f.advanceAttemptTo("running")
			f.recordEvidence(store.EvidenceRunningObserved)

			end(f)

			if got := f.attemptState(); got.State != "ready" {
				t.Errorf("attempt = %s (resolution %q); want it back in the queue",
					got.State, got.Resolution)
			}
		})
	}
}
