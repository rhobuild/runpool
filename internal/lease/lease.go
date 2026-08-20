// Package lease owns the lifecycle of the host resources one attempt
// consumes: the lease state machine, the durable resource-intent saga
// that removes every Docker object the capsule created, and the single
// transaction that ends both — release, cache-lane return and the
// attempt's disposition, committed together.
//
// It knows nothing about providers. A lease carries a binding id and the
// attempt it serves; who that binding is, and what has to be told about
// the runner, is the composition layer's business. That separation is
// the point: cleanup that has to reach a provider is cleanup that stops
// working when the provider is unreachable.
//
// Disposition rests on evidence alone, never on how far cleanup got. A
// lease state describes cleanup; reading it as an execution outcome is
// how a job that never ran gets settled as if it had.
package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// Remover is the slice of the Docker client cleanup drives intents to
// absent through. Removal of an already-absent object is success — that
// contract is what makes retrying a removal safe.
type Remover interface {
	RemoveOwnedContainer(ctx context.Context, reference string,
		instanceID assignment.InstanceID, leaseID assignment.LeaseID) error
	RemoveOwnedNetwork(ctx context.Context, reference string,
		instanceID assignment.InstanceID, leaseID assignment.LeaseID) error
	RemoveOwnedVolume(ctx context.Context, reference string,
		instanceID assignment.InstanceID, leaseID assignment.LeaseID) error
}

// Manager drives leases. One per controller; it holds no per-lease
// state, so every method is safe to call from a capsule's own goroutine.
type Manager struct {
	store  *store.Store
	remove Remover
	log    *slog.Logger
}

func NewManager(st *store.Store, remove Remover, log *slog.Logger) *Manager {
	return &Manager{store: st, remove: remove, log: log}
}

// Transition moves a lease along its state machine.
func (m *Manager) Transition(ctx context.Context, leaseID assignment.LeaseID, from, to store.LeaseState) error {
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.TransitionLease(leaseID, from, to)
	})
}

// RecordEvidence makes an observation about execution durable against
// the attempt the lease serves. It runs on a context detached from the
// job's: the observation must outlive a cancelled job, because it is
// what stops that job being run twice.
func (m *Manager) RecordEvidence(ctx context.Context, leaseID assignment.LeaseID, e store.Evidence) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.RecordEvidenceForLease(leaseID, e)
	})
}

// EvidenceOf reads what is known about the execution of the attempt a
// lease serves.
func (m *Manager) EvidenceOf(ctx context.Context, lease store.Lease) (store.Evidence, error) {
	var evidence store.Evidence
	err := m.store.Tx(ctx, func(tx *store.Tx) error {
		attempt, err := tx.Get(lease.AttemptID)
		if err != nil {
			return err
		}
		evidence = attempt.Evidence
		return nil
	})
	return evidence, err
}

// Release walks a live lease from `from` through cleaning to released,
// removing every owned resource on the way. A removal failure parks the
// lease in quarantined until a later reconciliation retries.
func (m *Manager) Release(ctx context.Context, leaseID assignment.LeaseID, from store.LeaseState) error {
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.TransitionLease(leaseID, from, store.LeaseDraining); err != nil {
			return err
		}
		return tx.TransitionLease(leaseID, store.LeaseDraining, store.LeaseCleaning)
	}); err != nil {
		return err
	}
	if err := m.RemoveResources(ctx, leaseID); err != nil {
		m.Quarantine(leaseID)
		return err
	}
	return m.Finalize(ctx, leaseID, "")
}

// ToCleaning moves a lease into cleaning from wherever a crash or a
// failure left it, and reports the state it was in. Beginning from the
// lease's actual state is the point: a lease already failed or
// quarantined cannot move to failed again, and attempting it once
// aborted the very cleanup those states exist to retry.
func (m *Manager) ToCleaning(ctx context.Context, leaseID assignment.LeaseID) (store.Lease, error) {
	var was store.Lease
	err := m.store.Tx(ctx, func(tx *store.Tx) error {
		lease, err := tx.LeaseByID(leaseID)
		if err != nil {
			return err
		}
		was = lease
		switch lease.State {
		case store.LeaseCleaning:
			return nil
		case store.LeaseFailed, store.LeaseQuarantined:
			return tx.TransitionLease(leaseID, lease.State, store.LeaseCleaning)
		default:
			if err := tx.TransitionLease(leaseID, lease.State, store.LeaseFailed); err != nil {
				return err
			}
			return tx.TransitionLease(leaseID, store.LeaseFailed, store.LeaseCleaning)
		}
	})
	return was, err
}

// FinishCleaning completes a release that was interrupted partway: the
// lease is already draining or cleaning, so its resources are removed
// and the finalizing transaction commits release, cache-lane return and
// attempt disposition together. A lease reaches cleaning from failure
// paths too, without a runner ever having existed, so the disposition
// comes from evidence, never from how far cleanup got.
func (m *Manager) FinishCleaning(ctx context.Context, lease store.Lease, obs assignment.ExecutionObservation) error {
	if lease.State == store.LeaseDraining {
		if err := m.Transition(ctx, lease.ID, store.LeaseDraining, store.LeaseCleaning); err != nil {
			return err
		}
	}
	if err := m.RemoveResources(ctx, lease.ID); err != nil {
		return err
	}
	return m.Finalize(ctx, lease.ID, obs)
}

// Finalize is the single transaction that ends a capsule's life.
// External cleanup has already finished while the lease was `cleaning`;
// here, atomically: no resource record may remain, the lease commits to
// released, the cache lane is freed in the books, and the attempt is
// disposed of by evidence. One commit, so no crash can separate a
// released lease from its attempt's disposition. Only after this commit
// may the admission credit be released.
func (m *Manager) Finalize(ctx context.Context, leaseID assignment.LeaseID, startObs assignment.ExecutionObservation) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		// A surviving resource record means external cleanup did not
		// finish: releasing anyway would free a credit whose privileged
		// containers may still exist.
		resources, err := tx.Resources(leaseID)
		if err != nil {
			return err
		}
		if len(resources) > 0 {
			return fmt.Errorf("lease %s still owns %d resource records; refusing to finalize", leaseID, len(resources))
		}
		lease, err := tx.LeaseByID(leaseID)
		if err != nil {
			return err
		}
		if err := tx.TransitionLease(leaseID, store.LeaseCleaning, store.LeaseReleased); err != nil {
			return err
		}
		if err := tx.ReleaseCacheLane(leaseID); err != nil {
			return err
		}
		return m.disposeAttempt(tx, lease, startObs)
	})
}

// disposeAttempt applies the evidence's ruling to the attempt a lease
// serves, inside the caller's transaction. A lease with no attempt is
// quietly complete — disposition already happened — which is what makes
// retrying the final transaction safe. Four rulings:
//
//   - execution was observed: settled, completed_observed or
//     started_observed by what the daemon showed;
//   - nothing was ever authorized to start: back to ready, because
//     nothing consumed the work;
//   - a start was authorized and the daemon proved it never took
//     effect: equally back to ready;
//   - a start was authorized and its outcome cannot be proven either
//     way: held for a person — settling it could drop a job that never
//     ran, requeueing it could run a job twice.
func (m *Manager) disposeAttempt(tx *store.Tx, lease store.Lease, startObs assignment.ExecutionObservation) error {
	attempt, err := tx.Get(lease.AttemptID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// Keyed by lease: an attempt is served once per lease, and a fixed key
	// would have the second serving's cleanup swallowed as a replay of the
	// first.
	if err := tx.RecordEvent(attempt.ID, "cleanup_completed:"+string(lease.ID), "cleanup_completed"); err != nil {
		return err
	}
	return m.applyDisposition(tx, attempt, lease.ID, startObs, dispositionFor(attempt, startObs))
}

// disposition is what the books must record about an attempt whose
// serving has ended.
//
// It is a value rather than a branch because two paths end a serving —
// the finalizing transaction and the sweep that finds a lease nobody is
// driving — and they must not be able to disagree. Expressed as two
// switches, the precedence between the rules lived in the order of the
// cases, which nothing checks: the same attempt could settle as a job
// that ran on one path and return to the queue on the other.
type disposition int

const (
	// dispositionNone is an attempt that is already resolved. The
	// serving still ends — the capsule is destroyed and the lease
	// released — but nothing is written against the attempt. Forcing a
	// disposition onto a resolved row matches no row, fails, and rolls
	// the release back with it, which pins the lease in cleaning holding
	// its admission credit for work somebody else already owns.
	dispositionNone disposition = iota
	// dispositionRequeue returns the workload to the queue: it provably
	// consumed nothing.
	dispositionRequeue
	// dispositionSettleCompleted and dispositionSettleStarted close the
	// attempt with what was observed of its execution.
	dispositionSettleCompleted
	dispositionSettleStarted
	// dispositionReview holds the attempt for a person: settling it
	// could drop a job that never ran, requeueing it could run one
	// twice.
	dispositionReview
)

// servedByThisLease reports whether an attempt is still this lease's to
// resolve: the five states one serving passes through, from the claim
// that created the lease to the runner owning the job.
//
// Every other state belongs to whoever put the attempt there — a
// redelivery that superseded it, a provider that canceled it, a person
// holding it for review, or an operator who resolved one back to
// `ready` — and a serving that ends afterwards leaves it alone.
//
// `ready` is deliberately absent, and its absence is what makes the
// requeue total: `leased`, `preparing` and `prepared` are exactly what
// RequeueAttempt accepts, and `starting` and `running` are exactly what
// RequeueProvenInertAttempt accepts, so every disposition this set
// admits matches a row. An attempt found in `ready` was put there by
// something else, and requeueing it again matches nothing — which fails
// the finalizing transaction and pins the lease in cleaning with its
// admission credit, for a workload that is already servable.
//
// It is not called "open": GetOpenAttemptByWorkload means a different,
// wider set — the seven states that block a redelivery, `manual_review`
// among them — and one word for two sets one grep apart is how the
// wrong one gets copied.
func servedByThisLease(state string) bool {
	switch state {
	case "leased", "preparing", "prepared", "starting", "running":
		return true
	}
	return false
}

// dispositionFor decides what one ended serving means for its attempt.
// The precedence is stated here, once, in the order these cases are
// written — but the cases are exhaustive over a value, so adding one
// cannot silently reorder the others the way inserting a switch case
// above its siblings did.
//
// The proof outranks the evidence. `running` and `running_observed` are
// written when the start authorization is accepted and the capsule
// reports itself up, never when a runner is seen owning a job, while
// ObservedCreated only ever comes from a proof: a runtime still in its
// created state, a supervisor reporting it never started, or the exit
// code the capsule reserves for exactly that.
func dispositionFor(attempt store.Attempt, obs assignment.ExecutionObservation) disposition {
	switch {
	case !servedByThisLease(attempt.State):
		return dispositionNone
	case obs == assignment.ObservedCreated:
		return dispositionRequeue
	case attempt.Evidence == store.EvidenceExitObserved:
		return dispositionSettleCompleted
	case attempt.Evidence == store.EvidenceRunningObserved:
		return dispositionSettleStarted
	case attempt.Evidence.Retriable():
		return dispositionRequeue
	case obs == assignment.ObservedRunning:
		// The runtime demonstrably began. This reaches the sweep when a
		// binding is gone and adoption was impossible.
		return dispositionSettleStarted
	default:
		return dispositionReview
	}
}

// applyDisposition writes one decision, inside the caller's transaction.
//
// It takes the observation only to report it. Both warnings carry the
// same four fields, because both answer the same question after the
// fact: which attempt, under which lease, how far it had got, and what
// the runtime was seen doing. On the review — the one disposition a
// person has to act on — the observation is the input their choice turns
// on, since absent is not the same answer as unobservable.
func (m *Manager) applyDisposition(tx *store.Tx, attempt store.Attempt,
	leaseID assignment.LeaseID, obs assignment.ExecutionObservation, d disposition) error {

	switch d {
	case dispositionNone:
		return nil
	case dispositionRequeue:
		m.log.Warn("the workload was not consumed; the attempt stays servable",
			"attempt", attempt.ID, "lease", leaseID, "state", attempt.State,
			"observation", string(obs))
		return m.requeueProven(tx, attempt, leaseID)
	case dispositionSettleCompleted:
		return tx.Settle(attempt.ID, attempt.State, assignment.ResolutionCompletedObserved)
	case dispositionSettleStarted:
		return tx.Settle(attempt.ID, attempt.State, assignment.ResolutionStartedObserved)
	default:
		m.log.Warn("start outcome is unproven; the attempt is held for review",
			"attempt", attempt.ID, "lease", leaseID, "state", attempt.State,
			"observation", string(obs))
		return tx.HoldForReview(attempt.ID, store.ReviewReasonStartOutcomeUnknown)
	}
}

// HoldAttempt takes an attempt out of the retry cycle and names why.
//
// It exists for the failures a retry cannot fix: the next attempt would
// launch the same configured image, meet the same answer, and spend
// another of the three servings finding out. Naming the reason is what
// turns three identical failures per job into one thing to change.
func (m *Manager) HoldAttempt(ctx context.Context, leaseID assignment.LeaseID, reason string) {
	m.withAttemptOfLease(ctx, leaseID, func(tx *store.Tx, attempt store.Attempt) error {
		m.log.Warn("holding the attempt for review", "attempt", attempt.ID,
			"lease", leaseID, "reason", reason)
		return tx.HoldForReview(attempt.ID, reason)
	})
}

// requeueProven returns an attempt whose start provably never took
// effect to the queue, choosing the requeue its state allows.
//
// The two exist because the plain requeue refuses everything past the
// start authorization — that refusal is the at-most-once rule — while
// this disposition arrives holding the one proof that outranks it. Which
// state the attempt is in by then is not a property of the failure: the
// walk advances it to `starting` and on to `running` optimistically, so
// the proof can land at either. Naming only one leaves the other
// matching no row, and a requeue that matches nothing rolls the whole
// finalizing transaction back and pins the lease in cleaning.
func (m *Manager) requeueProven(tx *store.Tx, attempt store.Attempt, leaseID assignment.LeaseID) error {
	return m.requeueOrReview(tx, attempt.ID, tx.RequeueServing(attempt), leaseID)
}

// requeueOrReview turns a refused retry into a review rather than an
// error. The budget is not a safety rule - the work provably never began
// either way - so the attempt is held for someone to look at, not
// settled and not retried forever.
func (m *Manager) requeueOrReview(tx *store.Tx, attemptID assignment.AttemptID, err error, leaseID assignment.LeaseID) error {
	if !errors.Is(err, store.ErrRetryBudgetExhausted) {
		return err
	}
	m.log.Warn("the attempt has used every serving its retry budget allows; holding it for review",
		"attempt", attemptID, "lease", leaseID)
	return tx.HoldForReview(attemptID, store.ReviewReasonRetryBudgetExhausted)
}

// DisposeStranded applies the same ruling as the finalizing transaction
// to an attempt whose lease already finished its own lifecycle. Release
// and disposition commit together, so this should be unreachable; it
// exists because an invariant nothing checks is an assumption.
//
// It decides through dispositionFor, which is the point: this path and
// disposeAttempt once carried their own switches and drifted apart.
func (m *Manager) DisposeStranded(ctx context.Context, lease store.Lease, obs assignment.ExecutionObservation) {
	m.withAttemptOfLease(ctx, lease.ID, func(tx *store.Tx, attempt store.Attempt) error {
		return m.applyDisposition(tx, attempt, lease.ID, obs, dispositionFor(attempt, obs))
	})
}

// withAttemptOfLease resolves the attempt a lease serves and runs one
// disposition against it in the same transaction. A lease with no
// attempt is quietly complete: disposition already happened.
func (m *Manager) withAttemptOfLease(ctx context.Context, leaseID assignment.LeaseID, fn func(*store.Tx, store.Attempt) error) {
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		attempt, err := tx.AttemptByLease(leaseID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return fn(tx, attempt)
	}); err != nil {
		m.log.Error("cannot dispose of the attempt of a released lease",
			"lease", leaseID, "error", err)
	}
}

// Quarantine parks a lease whose cleanup failed. It keeps consuming
// capacity until a later pass resolves it, which is the honest shape of
// "its privileged containers may still exist".
func (m *Manager) Quarantine(leaseID assignment.LeaseID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.TransitionLease(leaseID, store.LeaseCleaning, store.LeaseQuarantined)
	}); err != nil {
		// The lease stays wherever the failed transition left it, which is
		// still a live state, so the periodic reconciler keeps seeing it:
		// that pass works from ownership over every live state, not from
		// quarantine by name. What is lost is the record of why cleanup
		// gave up, and swallowing this error is how that becomes
		// invisible.
		m.log.Error("cannot quarantine the lease; it stays in its current state",
			"lease", leaseID, "error", err)
	}
}

// RemoveResources drives every one of a lease's intents to absent, in
// dependency order — containers before the network and volumes that
// hold them. Each removal walks its own intent: cleanup_pending, then
// deleting just before the call, then the row's deletion once absence
// is proven. A failure is booked on the intent with backoff, so the
// periodic reconciler retries that resource alone; the FK RESTRICT
// means a surviving intent keeps the lease un-releasable.
func (m *Manager) RemoveResources(ctx context.Context, leaseID assignment.LeaseID) error {
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkResourceCleanup(leaseID)
	}); err != nil {
		return err
	}
	var intents []store.ResourceIntent
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		intents, err = tx.Resources(leaseID)
		return err
	}); err != nil {
		return err
	}

	order := map[store.ResourceKind]int{
		store.ResourceContainer: 0,
		store.ResourceNetwork:   1,
		store.ResourceVolume:    2,
	}
	slices.SortFunc(intents, func(a, b store.ResourceIntent) int {
		return order[a.Kind] - order[b.Kind]
	})

	for _, in := range intents {
		if err := m.removeIntent(ctx, in); err != nil {
			return fmt.Errorf("remove %s %s: %w", in.Kind, in.Role, err)
		}
	}
	return nil
}

// removeIntent drives one intent to absent: deleting before the call,
// the removal addressed by the confirmed id or the deterministic name,
// and the row deleted only once absence is proven. A failure books
// bounded backoff on the intent and returns it.
func (m *Manager) removeIntent(ctx context.Context, in store.ResourceIntent) error {
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.MarkResourceDeleting(in.ID)
	}); err != nil {
		return err
	}
	if err := m.removeObject(ctx, in.Kind, in.Handle(), in.LeaseID); err != nil {
		if berr := m.store.Tx(ctx, func(tx *store.Tx) error {
			return tx.RecordResourceError(in.ID, err, time.Now().Add(removalBackoff(in.Retries)))
		}); berr != nil {
			m.log.Error("cannot book the removal failure", "intent", in.ID, "error", berr)
		}
		return err
	}
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.ForgetResource(in.ID)
	})
}

// removalBackoff paces retries per resource: exponential with jitter,
// capped, so a wedged object neither hammers the daemon nor waits
// forever.
func removalBackoff(retries int64) time.Duration {
	backoff := min(time.Duration(1<<min(retries, 6))*10*time.Second, 5*time.Minute)
	jitter := time.Duration(rand.Int64N(int64(backoff / 4)))
	return backoff + jitter
}

func (m *Manager) removeObject(ctx context.Context, kind store.ResourceKind,
	handle string, leaseID assignment.LeaseID) error {
	instanceID := m.store.InstanceID()
	switch kind {
	case store.ResourceContainer:
		return m.remove.RemoveOwnedContainer(ctx, handle, instanceID, leaseID)
	case store.ResourceNetwork:
		return m.remove.RemoveOwnedNetwork(ctx, handle, instanceID, leaseID)
	case store.ResourceVolume:
		return m.remove.RemoveOwnedVolume(ctx, handle, instanceID, leaseID)
	default:
		return fmt.Errorf("unknown resource kind %q", kind)
	}
}

// ForgetResource drops the intent of a Docker object that has just been
// removed by something else — the orphan sweep — matching by the
// confirmed id or the deterministic name, since an unconfirmed intent's
// object is only reachable by name. A lease with no matching intent is
// unaffected; the point is that a row never outlives its object.
func (m *Manager) ForgetResource(ctx context.Context, leaseID assignment.LeaseID, dockerID string) error {
	if leaseID == "" {
		return nil // never recorded, so nothing to reconcile
	}
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		intents, err := tx.Resources(leaseID)
		if err != nil {
			return err
		}
		for _, in := range intents {
			if in.DockerID == dockerID || in.Name == dockerID {
				return tx.ForgetResource(in.ID)
			}
		}
		return nil
	})
}

// IntentsDue reports whether every booked backoff on the lease's intents
// has elapsed. A lease with no intents is due by definition: only its
// finalizing transaction remains.
func (m *Manager) IntentsDue(ctx context.Context, leaseID assignment.LeaseID) (bool, error) {
	var due bool
	err := m.store.Tx(ctx, func(tx *store.Tx) error {
		intents, err := tx.Resources(leaseID)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, in := range intents {
			if in.NotBefore > now {
				return nil
			}
		}
		due = true
		return nil
	})
	return due, err
}

// WalkToRunning advances an adopted lease's bookkeeping to
// workload_running so the normal release path applies, whatever step the
// crash interrupted. It moves the lease machine only: what the workload
// did is the attempt's evidence, recorded from observation before this
// runs, and nothing here can fabricate it. States from draining onward,
// and failed/quarantined, are left for the caller's release path.
func (m *Manager) WalkToRunning(ctx context.Context, lease store.Lease) error {
	steps := map[store.LeaseState]store.LeaseState{
		store.LeaseReserved:          store.LeaseProvisioning,
		store.LeaseProvisioning:      store.LeaseRuntimeRegistered,
		store.LeaseRuntimeRegistered: store.LeaseWorkloadRunning,
	}
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		current, err := tx.LeaseByID(lease.ID)
		if err != nil {
			return err
		}
		for next, ok := steps[current.State]; ok; next, ok = steps[current.State] {
			if err := tx.TransitionLease(current.ID, current.State, next); err != nil {
				return err
			}
			current.State = next
		}
		return nil
	})
}

// Recorder returns the resource recorder for one lease. Each call it
// makes commits its own transaction: a plan must be durable before the
// create call runs, or the intent could not survive the crash it exists
// for.
//
// The concrete type is returned rather than the capsule's interface. The
// consumer declares that interface — capsule.ResourceRecorder — and Go
// satisfies it structurally at the call site, so this package produces a
// recorder without linking a container runtime to do it.
func (m *Manager) Recorder(ctx context.Context, leaseID assignment.LeaseID) *intentRecorder {
	return &intentRecorder{store: m.store, ctx: ctx, leaseID: leaseID}
}

type intentRecorder struct {
	store   *store.Store
	ctx     context.Context
	leaseID assignment.LeaseID
}

func (r *intentRecorder) Plan(kind, role, name string) (assignment.ResourceIntentID, error) {
	var id assignment.ResourceIntentID
	err := r.store.Tx(r.ctx, func(tx *store.Tx) error {
		var err error
		id, err = tx.PlanResource(r.leaseID, store.ResourceKind(kind), role, name)
		return err
	})
	return id, err
}

func (r *intentRecorder) Creating(intentID assignment.ResourceIntentID) error {
	return r.store.Tx(r.ctx, func(tx *store.Tx) error {
		return tx.MarkResourceCreating(intentID)
	})
}

func (r *intentRecorder) Confirm(intentID assignment.ResourceIntentID, dockerID string) error {
	return r.store.Tx(r.ctx, func(tx *store.Tx) error {
		return tx.MarkResourcePresent(assignment.ResourceIntentID(intentID), dockerID)
	})
}
