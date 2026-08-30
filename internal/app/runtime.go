package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/githubactions"
	"github.com/rhobuild/runpool/internal/lease"
	"github.com/rhobuild/runpool/internal/netsandbox"
	"github.com/rhobuild/runpool/internal/store"
)

// leaseExecutor owns one committed lease from capsule preparation through
// evidence-based disposition and resource cleanup. It does not discover work,
// assign capacity, or decide which interrupted leases require recovery.
type leaseExecutor struct {
	log          *slog.Logger
	store        *store.Store
	capsule      capsuleRuntime
	waiter       runtimeWaiter
	leases       *lease.Manager
	cache        *cache.LaneManager
	allocator    *allocator.Allocator
	ownership    *leaseOwnership
	network      *netsandbox.Manager
	cgroupDriver string
}

// capsuleRuntime is the execution surface the lease lifecycle consumes.
// The consumer-owned seam makes preparation, start, and inspection failures
// independently testable.
type capsuleRuntime interface {
	Prepare(ctx context.Context, spec capsule.Spec, rec capsule.ResourceRecorder) (capsule.PreparedRuntime, error)
	Start(ctx context.Context, prepared capsule.PreparedRuntime) error
	InspectExecution(ctx context.Context, prepared capsule.PreparedRuntime) (assignment.ExecutionObservation, error)
}

// runtimeWaiter is the daemon surface used to await and diagnose a capsule.
type runtimeWaiter interface {
	WaitExit(ctx context.Context, id string) (int64, error)
	TailLogs(ctx context.Context, id string, lines int) (string, error)
}

// createLease takes an admission credit for one ready attempt. The lease and the
// attempt's claim commit together, so the two can never disagree about
// whether a workload is being served.
func (e *leaseExecutor) createLease(ctx context.Context, b *binding, attempt store.Attempt) (store.Lease, error) {
	var lease store.Lease
	err := e.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		lease, err = tx.LeaseAttempt(attempt.ID, b.bindingID, b.tier.ID)
		return err
	})
	if err != nil {
		return store.Lease{}, err
	}
	e.log.Info("lease reserved", "binding", b.key, "lease", lease.ID,
		"project", attempt.TenantKey+"/"+attempt.ProjectKey, "attempt", attempt.ID)
	return lease, nil
}

// advanceAttempt walks the attempt machine one edge, best-effort: the
// walk is what an operator watches, while disposition rests on evidence
// and the terminal transitions, so a conflict is logged rather than
// fatal.
func (e *leaseExecutor) advanceAttempt(ctx context.Context, attemptID assignment.AttemptID, from, to store.AttemptState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := e.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.Advance(attemptID, from, to)
	}); err != nil {
		e.log.Warn("attempt did not advance", "attempt", attemptID,
			"from", from, "to", to, "error", err)
	}
}

// runCapsule drives one lease through the machine on its own context:
// cancelling serve stops admission, not a running job — drain waits, and
// whatever outlives the drain window is adopted on the next start.
func (e *leaseExecutor) runCapsule(b *binding, lease store.Lease) {
	attemptID := lease.AttemptID
	// startObs carries the classification of an ambiguous start into the
	// finalizing transaction. Recovery does not re-take it: the pass that
	// inspects a runtime only does so while the evidence is still
	// start_authorized, and an ambiguous start reaches here past that. So
	// it is recorded with the lease on the way into cleanup, and a retry
	// reads it back rather than measuring again.
	var startObs assignment.ExecutionObservation
	// The scheduler claims the lease before this goroutine is started, so the
	// claim is unbroken from the moment the lease row exists. Releasing is
	// registered before the credit defer, so it runs after it.
	defer e.ownership.release(lease.ID)
	// The credit is released by whoever reaches `released` — not here.
	// A lease that ends quarantined still owns privileged containers,
	// networks and volumes, so releasing its capacity would admit work
	// the host cannot actually run.
	defer e.releaseCreditIfDone(b, lease.ID)
	// The lease's own context outlives every step: cleanup and the
	// failure paths need one that is not the step's.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := e.log.With("binding", b.key, "lease", lease.ID)

	// Preparation and execution are bounded separately because they are
	// bounded for different reasons. Getting a capsule to the point of
	// running is this instance's own work — pull, boot, protocol, deliver
	// — and it either finishes in a known time or something is wrong
	// here. Waiting for the job is the provider's business, and the
	// tier's ceiling is the backstop for a capsule that stops reporting.
	// Wrapping both in the ceiling made its floor unreachable: a tier
	// configured at the lowest value the validator accepts would expire
	// inside the capsule's readiness budget, and the expiry would surface
	// as whichever preparation step it landed in.
	prepCtx, cancelPrep := context.WithTimeout(ctx, capsulePrepTimeout)
	defer cancelPrep()

	if err := e.leases.Transition(ctx, lease.ID, store.LeaseReserved, store.LeaseProvisioning); err != nil {
		// Unwound like every other failure in this function. Returning
		// bare left the lease reserved and its credit held until the
		// stranded grace elapsed, which is minutes of one tier's
		// capacity for a transient store error -- and the only step here
		// where that was the cost.
		log.Error("transition failed", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}

	runnerName := "runpool-" + engine.ShortID(string(lease.ID))
	jit, err := b.gh.GenerateJITConfig(prepCtx, b.scaleSetID, runnerName, workFolder)
	if err != nil {
		log.Error("jit generation failed", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	// The runtime's name is the lease's correlation handle; the runner id
	// GitHub assigned is the adapter's, and lands in the attempt's
	// metadata table where deregistration reads it back.
	if err := e.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.SetLeaseRuntimeName(lease.ID, assignment.RuntimeName(jit.RunnerName)); err != nil {
			return err
		}
		if err := tx.RecordGitHubRunnerID(attemptID, int64(jit.RunnerID)); err != nil {
			return err
		}
		return tx.TransitionLease(lease.ID, store.LeaseProvisioning, store.LeaseRuntimeRegistered)
	}); err != nil {
		log.Error("registering runner in state failed", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}

	// A repository-scoped binding leases an exclusive cache lane; the
	// lane is freed by lease teardown, so it survives a crash and is
	// reclaimed on the next reconciliation.
	var cacheMount capsule.CacheMount
	if b.cacheEnabled {
		loc, ok, err := e.cache.Acquire(prepCtx, b.ref.CanonicalURL, b.generation, lease.ID, b.maxLanes)
		switch {
		case err != nil:
			log.Warn("cache lane unavailable; running without cache", "error", err)
		case ok:
			cacheMount = capsule.CacheMount{Volume: loc.Volume}
			log.Info("cache lane leased",
				"project_id", loc.ProjectID, "generation", loc.Generation, "lane_id", loc.LaneID,
				"volume", loc.Volume)
		}
	}

	recorder := e.leases.Recorder(ctx, lease.ID)
	e.advanceAttempt(ctx, attemptID, store.AttemptLeased, store.AttemptPreparing)
	sandbox, err := e.network.ForLaunch(prepCtx)
	if err != nil {
		log.Error("network sandbox refresh failed", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	prepared, err := e.capsule.Prepare(prepCtx, capsule.Spec{
		LeaseID:      lease.ID,
		InstanceID:   e.store.InstanceID(),
		AttemptID:    attemptID,
		TargetID:     assignment.TargetID(b.target.ID),
		TierID:       assignment.TierID(b.tier.ID),
		CapsuleImage: b.capsuleImage,
		JITConfig:    jit.Encoded,
		Resources:    b.tier.Resources,
		Cache:        cacheMount,
		Sandbox:      sandbox,
		CgroupDriver: e.cgroupDriver,
	}, recorder)
	if err != nil {
		// Nothing was asked to run, so the assignment stays servable —
		// unless the image itself is the answer. A capsule that does not
		// speak this controller's protocol will not start speaking it on
		// the next attempt, so retrying spends the budget discovering the
		// same fact three times per job. That one is held with its reason
		// named, and the tier's image is the thing to change.
		log.Error("capsule preparation failed", "error", err)
		if errors.Is(err, capsule.ErrIncompatibleImage) {
			e.leases.HoldAttempt(ctx, lease.ID, store.ReviewReasonIncompatibleCapsule)
		}
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	// The gateway exists and is ready now, so this is the first moment
	// the policy it carries can be checked against the one in force --
	// and the last moment before the capsule is authorized to start, so
	// it is the only one at which a stale gateway is still free.
	if err := e.network.ConfirmLaunch(prepCtx, lease.ID, sandbox); err != nil {
		log.Error("the capsule's gateway does not carry the egress policy in force", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	// Recorded after the preparation exists — evidence names what
	// happened, never what was about to be attempted.
	if err := e.leases.RecordEvidence(ctx, lease.ID, store.EvidenceRuntimePrepared); err != nil {
		log.Error("cannot record that the runtime is prepared", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	e.advanceAttempt(ctx, attemptID, store.AttemptPreparing, store.AttemptPrepared)
	// This one edge is authoritative, unlike every other in the walk. It
	// is the last point before an effect that can begin execution, and
	// by here the attempt has been out of this goroutine's sight for the
	// whole preparation: a redelivery of the same workload can have
	// superseded it, and nothing between `prepared` and the start
	// consults it again. A compare-and-swap that matches no row means
	// something else resolved this attempt, and no start may be
	// authorized against it.
	//
	// It runs before the authorization is recorded rather than after,
	// which narrows the ambiguous window instead of widening it: a
	// compare-and-swap is not an effect, and an authorization recorded
	// for a start that will not be attempted is a claim that never
	// became true.
	//
	// It matches a set rather than the exact predecessor because the two
	// edges before it are best-effort: advanceAttempt logs a failure and
	// carries on, which is right for observability and wrong to depend
	// on. Requiring `prepared` made a transient store error tear down a
	// capsule that was ready to run.
	if err := e.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.AuthorizeStart(lease.ID, attemptID)
	}); err != nil {
		log.Warn("the attempt moved before the start was authorized; nothing is started",
			"attempt", attemptID, "error", err)
		// This controller's own knowledge, not the capsule's: no start
		// was ever issued, so the capsule has said nothing about one.
		// Spelling it as the capsule's account would put a report of
		// what the capsule said where the capsule was never asked, and
		// hand the provider's answer something to overrule that is not
		// the account it is allowed to overrule.
		e.recoverCapsuleFailure(ctx, b, lease.ID, assignment.ObservedNeverStarted)
		return
	}
	// The authorization is durable immediately before the one effect
	// that can begin execution, so the ambiguous window is exactly one
	// Docker request wide and a crash inside it is classified by
	// inspecting the runtime, never by guessing.
	if err := e.leases.RecordEvidence(ctx, lease.ID, store.EvidenceStartAuthorized); err != nil {
		log.Error("cannot record the start authorization", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	if startErr := e.capsule.Start(prepCtx, prepared); startErr != nil {
		// A failed Start call does not prove no start: the request may
		// have taken effect before the error. Classify from the daemon
		// now, before any cleanup can destroy the answer.
		obs, ierr := e.inspectExecution(ctx, prepared)
		if ierr != nil {
			log.Error("start outcome cannot be observed", "start_error", startErr, "inspect_error", ierr)
		}
		startObs = obs
		switch obs {
		case assignment.ObservedRunning:
			log.Warn("start reported an error but the runtime is running; continuing", "error", startErr)
		case assignment.ObservedExited:
			log.Warn("start reported an error but the runtime ran and exited", "error", startErr)
			if err := e.leases.RecordEvidence(ctx, lease.ID, store.EvidenceExitObserved); err != nil {
				log.Error("cannot record the observed exit", "error", err)
			}
			e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
			return
		default:
			report, unproven := startFailureReport(obs)
			if unproven {
				log.Error(report, "observation", string(obs), "error", startErr)
			} else {
				log.Info(report, "observation", string(obs), "error", startErr)
			}
			e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
			return
		}
	}
	if err := e.leases.RecordEvidence(ctx, lease.ID, store.EvidenceRunningObserved); err != nil {
		log.Error("cannot record that the runner is running", "error", err)
	}
	e.advanceAttempt(ctx, attemptID, store.AttemptStarting, store.AttemptRunning)
	if err := e.leases.Transition(ctx, lease.ID, store.LeaseRuntimeRegistered, store.LeaseWorkloadRunning); err != nil {
		log.Error("transition to running failed", "error", err)
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	runnerContainer := prepared.RuntimeID
	log.Info("capsule running", "runner", jit.RunnerName, "container", engine.ShortID(string(runnerContainer)))

	// From here the tier's ceiling governs, and only here: this is the
	// wait for work the provider owns, and the ceiling is the backstop
	// for a capsule that stops reporting rather than a limit on the job.
	// An adopted capsule spends what its lease has left of the same
	// budget, so a restart neither extends nor restarts it.
	waitCtx, cancelWait := context.WithTimeout(ctx, remainingCeiling(b.tier, lease.CreatedAt))
	defer cancelWait()
	exit, err := e.waiter.WaitExit(waitCtx, string(runnerContainer))
	if err == nil {
		// What the status proves, not that the wait returned. The
		// supervisor reserves one code for "the runner never owned the
		// job", and a clean wait carrying it is not an execution:
		// recording an observed exit settles an attempt that never ran
		// as complete, and nothing requeues it afterwards.
		if startObs = capsule.ClassifyExit(int(exit)); startObs != assignment.ObservedExited {
			log.Warn("the capsule reports the runner never started; the attempt returns to the "+
				"queue unless the provider says otherwise",
				"exit", exit)
			e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
			return
		}
		if err := e.leases.RecordEvidence(ctx, lease.ID, store.EvidenceExitObserved); err != nil {
			log.Error("cannot record that the runner completed", "error", err)
		}
	}
	if err != nil {
		// The ceiling and a broken wait unwind the same way, and only one
		// of them is a decision this instance made. Saying which is the
		// difference between an operator raising a tier's ceiling and one
		// hunting a capsule that never failed.
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			log.Error("capsule exceeded the tier's job ceiling; it is being destroyed",
				"tier", b.tier.ID, "ceiling", b.tier.Ceiling())
		} else {
			log.Error("waiting on runner failed", "error", err)
		}
		e.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	if exit != 0 {
		tail, _ := e.waiter.TailLogs(ctx, string(runnerContainer), 40)
		log.Warn("runner exited non-zero", "exit", exit, "log_tail", tail)
	} else {
		log.Info("runner exited cleanly")
	}

	if err := e.leases.Release(ctx, lease.ID, store.LeaseWorkloadRunning); err != nil {
		log.Error("cleanup failed; lease quarantined", "error", err)
		return
	}
	log.Info("lease released")
}

// releaseCreditIfDone returns the binding's admission credit only when the lease has
// reached its terminal state. A quarantined or otherwise stuck lease
// keeps consuming capacity until reconciliation or cleanup resolves it.
// creditReadAttempts and creditReadBackoff bound the one read that
// decides whether an admission credit comes back. The store is a single
// connection shared with every lease transition, every intent write, the
// disk monitor and the reconciler, so losing this read is what a moment
// of contention looks like rather than a lasting condition.
const (
	creditReadAttempts = 3
	creditReadBackoff  = 200 * time.Millisecond
)

func (e *leaseExecutor) releaseCreditIfDone(b *binding, leaseID assignment.LeaseID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Retried rather than given up on. This is the only producer of a
	// release for a lease that finished, and by here the lease is
	// released -- which puts it outside every later working set, because
	// those are built from live states. So a read that fails once and is
	// abandoned costs the binding that credit for the life of the
	// process, with one line in the log and nothing in status saying why.
	// The answer is durable and cannot change, so a retry can only find
	// the same one.
	var lease store.Lease
	var err error
	for attempt := range creditReadAttempts {
		if attempt > 0 {
			select {
			case <-time.After(creditReadBackoff):
			case <-ctx.Done():
			}
		}
		err = e.store.Tx(ctx, func(tx *store.Tx) error {
			var rerr error
			lease, rerr = tx.LeaseByID(leaseID)
			return rerr
		})
		if err == nil {
			break
		}
	}
	if err != nil {
		e.log.Error("cannot confirm lease disposition after retrying; holding its credit",
			"lease", leaseID, "attempts", creditReadAttempts, "error", err)
		return
	}
	if !lease.State.Terminal() {
		e.log.Warn("lease did not reach released; its credit stays held",
			"lease", leaseID, "state", string(lease.State))
		return
	}
	e.allocator.Release(b.key)
}

// recoverCapsuleFailure walks a lease to released through the failure path: external
// cleanup while the lease is cleaning, then the single finalizing
// transaction that also disposes of the attempt. A removal failure
// parks the lease in quarantined and reports it — the caller learns the
// lease is unresolved instead of assuming otherwise.
// b may be nil when the lease belongs to a target no longer configured:
// its Docker resources still need removing, and logging must not
// dereference it — a panic here takes down startup reconciliation for
// every other lease.
// recoverCapsuleFailure unwinds one lease after a failure, and disposes
// of its attempt.
//
// ctx bounds the recovery, and which context a caller passes is a
// statement about who else can finish this work. The serving path passes
// one of its own, detached from the job's: a recovery interrupted
// halfway leaves a lease nothing in this process will pick up again. The
// periodic reconciler passes the loop's, because its work is resumable
// by definition — the next pass, or the next start, finds the lease
// exactly where this left it — and a recovery that outlived the
// shutdown budget would have the platform kill the process inside one.
//
// startObs may be replaced here. The deregistration below is the one
// question this path puts to the party that assigned the work, and its
// answer outranks one account and one only: the capsule's own word that
// it never started, which is the account the job inside that capsule
// can write.
func (e *leaseExecutor) recoverCapsuleFailure(ctx context.Context, b *binding, leaseID assignment.LeaseID,
	startObs assignment.ExecutionObservation) error {

	ctx, cancel := context.WithTimeout(ctx, recoveryBudget)
	defer cancel()
	bindingKey := "(unconfigured)"
	if b != nil {
		bindingKey = string(b.key)
	}
	log := e.log.With("binding", bindingKey, "lease", leaseID)

	// After the drain window, recovery belongs to the successor. The
	// failures that arrive here now are the shutdown itself — the client
	// and store closing under a live wait — and acting on them would
	// rewrite a running lease to cleaning, handing the next start a
	// failure to dismantle instead of a capsule to adopt. A recovery
	// already past this point runs to completion: its transitions are
	// durable, and the successor resumes whatever it left.
	if e.ownership.isAbandoning() {
		// The lease is left as it is, but what this pass measured is not:
		// nothing re-takes it, and the successor arrives with an evidence
		// state past the one that would make it inspect. Recording is a
		// column rather than a state, so it leaves the successor a lease
		// to recover exactly as this branch intends.
		if err := e.leases.RecordStartObservation(ctx, leaseID, startObs); err != nil {
			log.Error("cannot record what this serving measured before abandoning it", "error", err)
		}
		log.Warn("drain window elapsed; the lease is left as it is for the next start to recover")
		return nil
	}

	was, err := e.leases.ToCleaning(ctx, leaseID)
	if err != nil {
		log.Error("failure transition failed", "error", err)
		return err
	}
	startObs = e.deregisterAndOverrule(ctx, b, was, startObs, log)

	// Before the first destructive step, and after every refinement above
	// that can overrule it. A recovery that cannot record what it saw must
	// not destroy the thing it saw: the lease stays cleaning, holding its
	// credit and its objects, and the periodic pass runs the whole
	// recovery again with the capsule still there.
	if err := e.leases.RecordStartObservation(ctx, leaseID, startObs); err != nil {
		log.Error("cannot record what this serving measured; nothing is removed", "error", err)
		return err
	}
	if err := e.leases.RemoveResources(ctx, leaseID); err != nil {
		log.Error("failure cleanup failed; lease quarantined", "error", err)
		e.leases.Quarantine(leaseID)
		return err
	}
	if err := e.leases.Finalize(ctx, leaseID, startObs); err != nil {
		log.Error("failure finalization failed; the lease stays cleaning for reconciliation", "error", err)
		return err
	}
	return nil
}

// startFailureReport says what a failed start's observation means for
// the assignment, and whether the outcome is one nobody established.
//
// It is a function rather than the last branch of the switch above
// because a branch that catches everything left over says the same
// thing about a value nobody thought about as it does about the values
// it was written for. That is how the daemon's own account of a
// container it never started came to be reported as an outcome needing
// an operator, at the level an operator is paged on, for the ordinary
// case of a start that failed and left nothing behind.
//
// An observation with no report here is a value nobody has decided
// about, which TestEveryObservationHasAStartFailureReport fails on.
func startFailureReport(obs assignment.ExecutionObservation) (report string, unproven bool) {
	switch obs {
	case assignment.ObservedNeverStarted:
		return "the daemon reports the container was never started; the assignment stays servable", false
	case assignment.ObservedCreated:
		return "the capsule reports the runner never started; the attempt returns to the " +
			"queue unless the provider says otherwise", false
	case assignment.ObservedAbsent, assignment.ObservedUnavailable:
		return "start outcome is unobservable; the assignment needs an operator", true
	case assignment.ObservedRunning, assignment.ObservedExited:
		// Decided before this is reached, and named here so the totality
		// check is about every observation rather than the leftovers.
		return "the runtime outlived the start that reported an error", false
	case assignment.NoObservation:
		// Distinct from the pair above it: those were asked and could not
		// answer, this was never asked. Both need an operator, and only
		// one of them says the runtime was reached.
		return "no observation was taken of this start; the assignment needs an operator", true
	}
	// A value this build does not declare. Naming what it means would be
	// inventing one, and that is the mistake this function exists to stop.
	// Saying nothing is a different mistake: the caller logs this as the
	// message, so an empty one pages an operator with a line that has no
	// message to filter, alert or search on.
	return "the start failed carrying an observation this build does not declare", true
}

// remainingCeiling is what is left of a lease's tier ceiling.
//
// The ceiling bounds how long this instance waits for one capsule, and a
// capsule adopted after a restart has already spent part of it. Measuring
// from the lease's own start is what keeps a restart from handing a
// wedged capsule a fresh full budget — the failure mode the ceiling
// exists to bound would otherwise be extended by every restart. A lease
// already past its ceiling gets a short grace rather than zero, so the
// wait resolves through the ordinary path instead of expiring before it
// begins.
func remainingCeiling(tier config.Tier, createdAt time.Time) time.Duration {
	const grace = time.Minute
	if createdAt.IsZero() {
		return tier.Ceiling()
	}
	if left := tier.Ceiling() - time.Since(createdAt); left > grace {
		return left
	}
	return grace
}

// inspectExecution bounds one observation of a runtime.
//
// The observation is a `docker exec` into the capsule, and an exec runs
// until its context ends: nothing in the Docker API cancels one. Both
// callers hold something open while they wait — this one is a launch
// goroutine the drain counts, and the other is startup, before any loop
// has begun. Handed a context with no deadline, a daemon that accepted
// the call and stopped answering makes a wedged shutdown out of a slow
// one.
func (e *leaseExecutor) inspectExecution(ctx context.Context,
	prepared capsule.PreparedRuntime) (assignment.ExecutionObservation, error) {

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	return e.capsule.InspectExecution(ctx, prepared)
}

// deregisterAndOverrule removes the provider's registration for an
// attempt and lets what the provider says outrank what the capsule said
// about itself.
//
// It is a function because two paths end a serving and both need it. The
// failure path called it inline; the recovery that resumes an
// interrupted release did not, so a capsule that forged "I never
// started" was believed there -- and the threat model promised, without
// naming a path, that such a forgery cannot return an assignment to the
// queue while the provider still holds the runner busy. That promise is
// the one protection stated against the one surface the design admits a
// job can forge, and it is worth making true everywhere rather than
// narrowing.
func (e *leaseExecutor) deregisterAndOverrule(ctx context.Context, b *binding,
	lease store.Lease, obs assignment.ExecutionObservation,
	log *slog.Logger) assignment.ExecutionObservation {

	// The read-back is inside, ahead of the question, and not left to
	// each caller to remember. A retry of a recovery arrives with nothing
	// measured, because the pass that measured it is the one that failed
	// -- so the observation this asks the provider about must already be
	// the recorded one, or the provider is asked about an absence and its
	// answer is discarded for not matching. Done in the callers, one of
	// them had it and one did not, and the one that did not heard a
	// refusal and requeued anyway.
	if !obs.Establishes() && lease.StartObservation.Establishes() {
		obs = lease.StartObservation
	}

	// The runner id lives in the adapter's metadata for the attempt this
	// lease serves; a binding of another provider has none, and zero
	// means there is nothing to deregister. Reaching for it is this
	// layer's job precisely because the lease machine must not know a
	// provider exists.
	var runnerGitHubID int64
	if err := e.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		runnerGitHubID, err = tx.GitHubRunnerID(lease.AttemptID)
		return err
	}); err != nil {
		log.Warn("cannot read the registered runner id; skipping deregistration", "error", err)
	}

	// b is nil for a lease whose target is no longer configured: clean
	// its Docker resources, but there is no client to deregister the
	// runner with — GitHub expires an unseen ephemeral runner on its own.
	if runnerGitHubID != 0 && b != nil {
		switch err := b.gh.RemoveRunner(ctx, int(runnerGitHubID)); {
		case err == nil:
		case errors.Is(err, githubactions.ErrRunnerNotFound):
			// Already gone, which is what cleanup wanted.
			log.Debug("the runner registration was already removed", "runner", runnerGitHubID)
		case errors.Is(err, githubactions.ErrJobStillRunning):
			// The provider still holds this runner busy, so the
			// registration outlives the capsule that carried it and
			// counts against the scale set until the provider expires it.
			log.Warn("the provider refuses to deregister a runner it still considers busy; "+
				"the registration is leaked until it expires there",
				"runner", runnerGitHubID, "error", err)
			// And it says something the capsule cannot be trusted to
			// say. The capsule's own account of never having handed the
			// job over -- the state it writes, the status it exits with
			// -- is produced inside the machine running the job, whose
			// daemon socket that job holds. This comes from the party
			// that assigned the work, which still considers the runner
			// busy with it.
			//
			// Only that one account is replaced. What the host daemon
			// says is not the capsule's word and is not the weaker of
			// the two: a container it has never started is a container
			// in which nothing has run, observed from outside, more
			// recently and closer to hand. And an outcome nobody could
			// establish stays held for a person, because this does not
			// establish it either -- it says a runner was busy, not that
			// this attempt's runner ran.
			if obs == assignment.ObservedCreated {
				log.Warn("the provider says the job was handed over; the capsule's own account "+
					"of never having started it is not what settles this",
					"observation", string(obs))
				obs = assignment.ObservedRunning
			}

		default:
			log.Warn("removing registered runner", "runner", runnerGitHubID, "error", err)
		}
	}
	return obs
}
