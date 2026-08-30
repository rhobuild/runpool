package lease

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
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

// recordingRemover keeps every removal it was asked for, with the
// ownership scope it was asked under. The fakes before it declared those
// parameters and discarded them, so a removal that blanked its lease id
// — or swapped in another instance's — reached the daemon unnoticed by
// every hermetic test: the adapter's refusal is proved live, and the
// caller's wiring was proved by nothing.
type recordingRemover struct {
	calls []removal
}

type removal struct {
	kind     store.ResourceKind
	handle   string
	instance assignment.InstanceID
	lease    assignment.LeaseID
}

func (r *recordingRemover) record(kind store.ResourceKind, handle string, i assignment.InstanceID, l assignment.LeaseID) error {
	r.calls = append(r.calls, removal{kind, handle, i, l})
	return nil
}
func (r *recordingRemover) RemoveOwnedContainer(_ context.Context, h string, i assignment.InstanceID, l assignment.LeaseID) error {
	return r.record(store.ResourceContainer, h, i, l)
}
func (r *recordingRemover) RemoveOwnedNetwork(_ context.Context, h string, i assignment.InstanceID, l assignment.LeaseID) error {
	return r.record(store.ResourceNetwork, h, i, l)
}
func (r *recordingRemover) RemoveOwnedVolume(_ context.Context, h string, i assignment.InstanceID, l assignment.LeaseID) error {
	return r.record(store.ResourceVolume, h, i, l)
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
		if _, err := tx.RecordDelivery(binding, "msg-1", []assignment.WorkloadAssignment{{
			SourceWorkloadKey: "payload",
		}},
			[]store.WorkloadRow{{SourceWorkloadKey: "job-1", TenantKey: "acme", ProjectKey: "app"}}); err != nil {
			return err
		}
		ready, err := tx.AllReadyAttempts(binding)
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
func (f *fixture) advanceAttemptTo(target store.AttemptState) {
	f.t.Helper()
	ladder := []store.AttemptState{store.AttemptLeased, store.AttemptPreparing,
		store.AttemptPrepared, store.AttemptStarting, store.AttemptRunning}
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
		wantState      store.AttemptState
		wantResolution assignment.Resolution
	}{
		{"exit observed", store.EvidenceExitObserved, store.LeaseCleaning, assignment.NoObservation, false, store.AttemptSettled, assignment.ResolutionCompletedObserved},
		{"running observed", store.EvidenceRunningObserved, store.LeaseCleaning, assignment.NoObservation, false, store.AttemptSettled, assignment.ResolutionStartedObserved},
		{"nothing was prepared", store.EvidenceNotStarted, store.LeaseCleaning, assignment.NoObservation, false, store.AttemptReady, ""},
		{"prepared but never authorized", store.EvidenceRuntimePrepared, store.LeaseCleaning, assignment.NoObservation, false, store.AttemptReady, ""},
		{"authorized and unprovable", store.EvidenceStartAuthorized, store.LeaseCleaning, assignment.ObservedAbsent, false, store.AttemptManualReview, ""},
		{"authorized and proven inert", store.EvidenceStartAuthorized, store.LeaseCleaning, assignment.ObservedCreated, false, store.AttemptReady, ""},
		{"lease not yet cleaning", store.EvidenceNotStarted, store.LeaseQuarantined, assignment.NoObservation, true, store.AttemptLeased, ""},
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
		id, err := tx.PlanResource(f.lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-leftover")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(assignment.ResourceIntentID(id), "leftover")
	})

	if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.NoObservation); err == nil {
		t.Fatal("finalizing with a surviving resource record succeeded")
	}
	if got := f.reload(); got.State != store.LeaseCleaning {
		t.Errorf("lease = %s; want still cleaning — nothing may commit", got.State)
	}
	if got := f.attemptState(); got.State != store.AttemptLeased {
		t.Errorf("attempt = %s; want still leased — nothing may commit", got.State)
	}
}

// Release walks a live lease all the way to released, removing every
// owned object on the way and disposing of the attempt by evidence.
func TestReleaseRemovesEverythingAndDisposes(t *testing.T) {
	remover := &recordingRemover{}
	f := newFixture(t, remover)
	f.driveTo(store.LeaseWorkloadRunning)
	f.recordEvidence(store.EvidenceExitObserved)
	// The full shape of a sandboxed lease, not one container: the order
	// is only observable with something to order, and every test that
	// reached the removal planned exactly one intent — so inverting the
	// sort, or deleting it, failed nothing, while a release that removes
	// a network before the containers holding it is refused by the
	// daemon on every pass and quarantines the lease forever.
	f.tx(func(tx *store.Tx) error {
		for _, in := range []struct {
			kind store.ResourceKind
			role store.ResourceRole
			name string
		}{
			{store.ResourceNetwork, store.ResourceRoleCapsuleNetwork, "runpool-net-1"},
			{store.ResourceVolume, store.ResourceRoleDindData, "runpool-dind-1"},
			{store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-1"},
			{store.ResourceContainer, store.ResourceRoleGateway, "runpool-gateway-1"},
		} {
			id, err := tx.PlanResource(f.lease.ID, in.kind, in.role, in.name)
			if err != nil {
				return err
			}
			if err := tx.MarkResourcePresent(assignment.ResourceIntentID(id), in.name); err != nil {
				return err
			}
		}
		return nil
	})

	if err := f.m.Release(t.Context(), f.lease.ID, store.LeaseWorkloadRunning); err != nil {
		t.Fatal(err)
	}
	if got := f.reload(); got.State != store.LeaseReleased {
		t.Errorf("lease = %s; want released", got.State)
	}

	if len(remover.calls) != 4 {
		t.Fatalf("removed %d objects; want all 4", len(remover.calls))
	}
	if remover.calls[0].kind != store.ResourceContainer {
		t.Errorf("removed a %s first; containers go first, because the network and the "+
			"volumes they hold cannot be removed under them", remover.calls[0].kind)
	}
	instance := f.m.store.InstanceID()
	for _, c := range remover.calls {
		if c.instance != instance || c.lease != f.lease.ID {
			t.Errorf("removed %s %q as instance %q lease %q; want %q and %q — the scope is "+
				"what keeps a stale intent from deleting a foreign object that reused its name",
				c.kind, c.handle, c.instance, c.lease, instance, f.lease.ID)
		}
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
	if got := f.attemptState(); got.State != store.AttemptSettled {
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
		id, err := tx.PlanResource(f.lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-wedge")
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
	if got := f.attemptState(); got.State != store.AttemptLeased {
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
	if err := f.m.FinishCleaning(t.Context(), f.reload(), assignment.NoObservation); err != nil {
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

	id, err := rec.Plan("container", "capsule", "runpool-runner-x")
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
		if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.NoObservation); err != nil {
			t.Fatalf("finalize serving %d: %v", serving, err)
		}
		var attempt store.Attempt
		f.tx(func(tx *store.Tx) error {
			var err error
			attempt, err = tx.Get(assignment.AttemptID(f.attempt))
			return err
		})
		if attempt.State == store.AttemptManualReview {
			if attempt.ReviewReason != store.ReviewReasonRetryBudgetExhausted {
				t.Fatalf("held for review as %q; want %q, so an operator knows the "+
					"question is whether retrying will ever stop",
					attempt.ReviewReason, store.ReviewReasonRetryBudgetExhausted)
			}
			return
		}
		if attempt.State != store.AttemptReady {
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
	f.advanceAttemptTo(store.AttemptRunning)
	f.recordEvidence(store.EvidenceRunningObserved)

	if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.ObservedCreated); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if got := f.attemptState(); got.State != store.AttemptReady {
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

	if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.NoObservation); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got := f.reload().State; got != store.LeaseReleased {
		t.Errorf("lease = %s; want released — a held attempt must not pin the lease serving it", got)
	}
	if got := f.attemptState(); got.State != store.AttemptManualReview {
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
		state    store.AttemptState
		evidence store.Evidence
		// stored is what the serving recorded when it measured, which a
		// retry of its cleanup has to read back: the pass that measured is
		// the one that failed, and it arrives here having measured nothing.
		stored assignment.ExecutionObservation
		obs    assignment.ExecutionObservation
		want   disposition
	}{
		"proven inert outranks a running observation": {
			store.AttemptRunning, store.EvidenceRunningObserved, assignment.NoObservation, assignment.ObservedCreated, dispositionRequeue},
		// The three below are the retry of the row above: its cleanup
		// failed, so the pass that measured the proof is not the pass that
		// disposes of the attempt. What the retry carries establishes
		// nothing -- it either measured nothing or found the capsule this
		// cleanup is removing already gone -- and without the recorded
		// proof each of them settles a job that never ran.
		"a recorded proof outranks a retry that found nothing": {
			store.AttemptRunning, store.EvidenceRunningObserved,
			assignment.ObservedCreated, assignment.ObservedAbsent, dispositionRequeue},
		"a recorded proof outranks a retry that measured nothing": {
			store.AttemptRunning, store.EvidenceRunningObserved,
			assignment.ObservedCreated, assignment.NoObservation, dispositionRequeue},
		// And a later measurement that does establish something still
		// wins: the provider is asked again on the retry, and it is the
		// one account entitled to overrule the capsule.
		"a later establishing measurement outranks the recorded proof": {
			store.AttemptRunning, store.EvidenceRunningObserved,
			assignment.ObservedCreated, assignment.ObservedRunning, dispositionSettleStarted},
		"an observed exit settles as completed": {
			store.AttemptRunning, store.EvidenceExitObserved, assignment.NoObservation, assignment.NoObservation, dispositionSettleCompleted},
		// Nothing measured and nothing recorded: the tier ceiling, where
		// the capsule is destroyed while the runner is still working. That
		// job really did run.
		"a running observation settles as started": {
			store.AttemptRunning, store.EvidenceRunningObserved, assignment.NoObservation, assignment.NoObservation, dispositionSettleStarted},
		"nothing prepared returns to the queue": {
			store.AttemptLeased, store.EvidenceNotStarted, assignment.NoObservation, assignment.NoObservation, dispositionRequeue},
		"a running runtime settles as started": {
			store.AttemptStarting, store.EvidenceStartAuthorized, assignment.NoObservation, assignment.ObservedRunning, dispositionSettleStarted},
		"an unprovable start is held": {
			store.AttemptStarting, store.EvidenceStartAuthorized, assignment.NoObservation, assignment.ObservedAbsent, dispositionReview},
		"an attempt an operator returned to the queue is left alone": {
			store.AttemptReady, store.EvidenceNotStarted, assignment.NoObservation, assignment.NoObservation, dispositionNone},
		"a superseded attempt is left alone": {
			store.AttemptSuperseded, store.EvidenceNotStarted, assignment.NoObservation, assignment.NoObservation, dispositionNone},
		"a held attempt is left alone": {
			store.AttemptManualReview, store.EvidenceNotStarted, assignment.NoObservation, assignment.NoObservation, dispositionNone},
		"a settled attempt is left alone": {
			store.AttemptSettled, store.EvidenceExitObserved, assignment.NoObservation, assignment.NoObservation, dispositionNone},
	} {
		t.Run(name, func(t *testing.T) {
			attempt := store.Attempt{State: tc.state, Evidence: tc.evidence}
			lease := store.Lease{StartObservation: tc.stored}
			if got, _ := dispositionFor(lease, attempt, tc.obs); got != tc.want {
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
			f.advanceAttemptTo(store.AttemptRunning)
			f.recordEvidence(store.EvidenceRunningObserved)

			end(f)

			if got := f.attemptState(); got.State != store.AttemptReady {
				t.Errorf("attempt = %s (resolution %q); want it back in the queue",
					got.State, got.Resolution)
			}
		})
	}
}

// TestEveryServingStateCanBeDisposedOf: no disposition this decision can
// reach fails to match a row.
//
// The requeue is split across two queries whose guards between them
// cover exactly the five states one serving passes through. A state
// admitted to the set but absent from both guards produces a requeue
// that matches nothing, and the finalizing transaction rolls back with
// it — the lease pins in cleaning holding its admission credit, and a
// lease already in cleaning cannot move there again, so no later pass
// recovers it.
//
// The states walked are the store's own list filtered through the
// predicate, so neither side of the question is restated here: the
// domain comes from store.AttemptStates, which its own test holds
// against the schema's constraint, and the answer comes from
// servedByThisLease. A state added to the serving set is disposed of
// here whether or not anyone remembers this test, and one the fixture
// cannot drive to fails in advanceAttemptTo, which names it.
func TestEveryServingStateCanBeDisposedOf(t *testing.T) {
	var walked int
	for _, state := range store.AttemptStates() {
		if !servedByThisLease(state) {
			continue
		}
		walked++
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t, nopRemover{})
			f.driveTo(store.LeaseCleaning)
			if state != store.AttemptLeased {
				f.advanceAttemptTo(state)
			}

			// Retriable evidence is the disposition that reaches the
			// requeue from every one of these states.
			if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.NoObservation); err != nil {
				t.Fatalf("Finalize from %s: %v", state, err)
			}
			if got := f.reload().State; got != store.LeaseReleased {
				t.Errorf("lease = %s after disposing an attempt in %s; want released", got, state)
			}
		})
	}
	if walked == 0 {
		t.Fatal("the serving set admits no state; this test asserted nothing")
	}
}

// TestAnAttemptReturnedToTheQueueDoesNotPinItsLease: an operator can
// resolve a held attempt back to `ready` while the lease that served it
// is still cleaning up, and that serving must still end.
func TestAnAttemptReturnedToTheQueueDoesNotPinItsLease(t *testing.T) {
	f := newFixture(t, nopRemover{})
	f.driveTo(store.LeaseCleaning)
	id := assignment.AttemptID(f.attempt)
	f.tx(func(tx *store.Tx) error {
		if err := tx.HoldForReview(id, store.ReviewReasonRetryBudgetExhausted); err != nil {
			return err
		}
		return tx.ResolveReviewToReady(id, "resolved", "operator")
	})

	if err := f.m.Finalize(t.Context(), f.lease.ID, assignment.NoObservation); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got := f.reload().State; got != store.LeaseReleased {
		t.Errorf("lease = %s; want released — an attempt already servable must not pin it", got)
	}
	if got := f.attemptState().State; got != store.AttemptReady {
		t.Errorf("attempt = %s; want it left where the operator put it", got)
	}
}

// TestARecordedProofSurvivesAFailedCleanup is the sequence the record
// exists for.
//
// A capsule reports that the runner never owned the job, so the
// attempt must return to the queue even though the capsule had already
// reported itself up. Cleanup then fails against a wedged daemon and the
// lease is quarantined -- the documented retry path. The pass that
// measured the proof is therefore not the pass that disposes of the
// attempt, and the retry arrives having measured nothing: absent,
// because the capsule a measurement would have come from is the one this
// cleanup is removing.
//
// Without the record the retry settles the attempt as one that ran, and
// the workload is never served again while the books say it started.
func TestARecordedProofSurvivesAFailedCleanup(t *testing.T) {
	remover := &wedgedRemover{}
	f := newFixture(t, remover)
	f.driveTo(store.LeaseDraining)
	f.advanceAttemptTo(store.AttemptRunning)
	f.tx(func(tx *store.Tx) error {
		if err := tx.RecordEvidence(f.lease.AttemptID, store.EvidenceRunningObserved); err != nil {
			return err
		}
		id, err := tx.PlanResource(f.lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-proof")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(assignment.ResourceIntentID(id), "proof-1")
	})

	// The pass that measured the proof cannot finish: the daemon is wedged.
	if err := f.m.FinishCleaning(t.Context(), f.reload(), assignment.ObservedCreated); err == nil {
		t.Fatal("cleanup against a wedged daemon reported success")
	}
	if got := f.reload().StartObservation; got != assignment.ObservedCreated {
		t.Fatalf("the serving recorded %q; the proof has to outlive the pass that took it", got)
	}

	// The daemon heals and the retry runs, having measured nothing.
	remover.healed = true
	if err := f.m.FinishCleaning(t.Context(), f.reload(), assignment.ObservedAbsent); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	got := f.attemptState()
	if got.State != store.AttemptReady {
		t.Errorf("attempt = %s; want ready — the capsule proved the runner never owned this job", got.State)
	}
	if got.Resolution != "" {
		t.Errorf("attempt resolution = %q; a job that never ran is not settled", got.Resolution)
	}
}

// TestOnlyAMeasurementIsRecorded holds the predicate in Go and the
// column's own constraint to one rule. They are two statements of it in
// different languages, and both directions of drift are a real failure:
// a value that establishes nothing written over a proof spends it, and a
// value that does establish something refused by the database aborts a
// recovery in the middle and pins a lease that should have released.
func TestOnlyAMeasurementIsRecorded(t *testing.T) {
	// The list is every value of the vocabulary, NoObservation included,
	// so this covers the value a retry arrives with as well as the
	// measurements.
	for _, obs := range assignment.ExecutionObservations() {
		name := string(obs)
		if obs == assignment.NoObservation {
			name = "nothing measured"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nopRemover{})
			if err := f.m.RecordStartObservation(t.Context(), f.lease.ID, obs); err != nil {
				t.Fatalf("recording %q: %v", obs, err)
			}
			want := obs
			if !obs.Establishes() {
				want = assignment.NoObservation
			}
			if got := f.reload().StartObservation; got != want {
				t.Errorf("the serving recorded %q; want %q", got, want)
			}
		})
	}
}

// TestTheColumnRefusesAnEmptyMeasurement: the storage represents a
// serving that recorded nothing as NULL, and the empty string is not a
// second way to spell it. Two representations of one state is what a
// reader of the table would have to know about and nothing would tell
// them, so the column refuses one of them outright -- below the Go guard
// rather than beside it, because a guard that can be removed is not what
// keeps a table honest.
func TestTheColumnRefusesAnEmptyMeasurement(t *testing.T) {
	f := newFixture(t, nopRemover{})
	err := f.m.store.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.RecordLeaseStartObservation(f.lease.ID, "")
	})
	if err == nil {
		t.Fatal("the column accepted an empty measurement; NULL and the empty string now both mean nothing")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("the write failed with %v; the column's own constraint is what has to refuse it", err)
	}
}

// TestTheColumnRefusesAnEmptyRuntimeName: a lease that has registered no
// runtime has no name, and the lookup that answers "which attempt ran as
// this runtime" searches by that value. An empty name stored here would
// be a name a caller could ask for, matching a lease that registered
// nothing -- so the column refuses it and the absence is NULL.
func TestTheColumnRefusesAnEmptyRuntimeName(t *testing.T) {
	f := newFixture(t, nopRemover{})
	err := f.m.store.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.SetLeaseRuntimeName(f.lease.ID, "")
	})
	if err == nil {
		t.Fatal("the column accepted an empty runtime name; a lookup for one would match a lease that registered nothing")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("the write failed with %v; the column's own constraint is what has to refuse it", err)
	}
}
