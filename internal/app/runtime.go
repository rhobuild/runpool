package app

import (
	"context"
	"errors"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

// createLease takes an admission credit for one ready attempt. The lease and the
// attempt's claim commit together, so the two can never disagree about
// whether a workload is being served.
func (s *Controller) createLease(ctx context.Context, b *binding, attempt store.Attempt) (store.Lease, error) {
	var lease store.Lease
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		lease, err = tx.LeaseAttempt(attempt.ID, b.bindingID, b.tier.ID)
		return err
	})
	if err != nil {
		return store.Lease{}, err
	}
	s.log.Info("lease reserved", "binding", b.key, "lease", lease.ID,
		"project", attempt.TenantKey+"/"+attempt.ProjectKey, "attempt", attempt.ID)
	return lease, nil
}

// advanceAttempt walks the attempt machine one edge, best-effort: the
// walk is what an operator watches, while disposition rests on evidence
// and the terminal transitions, so a conflict is logged rather than
// fatal.
func (s *Controller) advanceAttempt(ctx context.Context, attemptID assignment.AttemptID, from, to store.AttemptState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.Advance(attemptID, from, to)
	}); err != nil {
		s.log.Warn("attempt did not advance", "attempt", attemptID,
			"from", from, "to", to, "error", err)
	}
}

// runCapsule drives one lease through the machine on its own context:
// cancelling serve stops admission, not a running job — drain waits, and
// whatever outlives the drain window is adopted on the next start.
func (s *Controller) runCapsule(b *binding, lease store.Lease) {
	defer s.wg.Done()
	attemptID := lease.AttemptID
	// startObs carries the classification of an ambiguous start into the
	// finalizing transaction. It lives in memory because it is taken
	// before cleanup destroys the container that proves it; after a
	// crash, recovery re-takes the same observation from the daemon.
	var startObs assignment.ExecutionObservation
	// The scheduler claims the lease before this goroutine is started, so
	// the claim is unbroken from the moment the lease row exists. Claiming
	// again is a no-op that covers a direct caller; releasing is registered
	// before the credit defer, so it runs after it.
	s.claimLease(lease.ID)
	defer s.releaseLease(lease.ID)
	// The credit is released by whoever reaches `released` — not here.
	// A lease that ends quarantined still owns privileged containers,
	// networks and volumes, so releasing its capacity would admit work
	// the host cannot actually run.
	defer s.releaseCreditIfDone(b, lease.ID)
	// The lease's own context outlives every step: cleanup and the
	// failure paths need one that is not the step's.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := s.log.With("binding", b.key, "lease", lease.ID)

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

	if err := s.leases.Transition(ctx, lease.ID, store.LeaseReserved, store.LeaseProvisioning); err != nil {
		log.Error("transition failed", "error", err)
		return
	}

	runnerName := "runpool-" + docker.ShortID(string(lease.ID))
	jit, err := b.gh.GenerateJITConfig(prepCtx, b.scaleSetID, runnerName, workFolder)
	if err != nil {
		log.Error("jit generation failed", "error", err)
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	// The runtime's name is the lease's correlation handle; the runner id
	// GitHub assigned is the adapter's, and lands in the attempt's
	// metadata table where deregistration reads it back.
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.SetLeaseRuntimeName(lease.ID, assignment.RuntimeName(jit.RunnerName)); err != nil {
			return err
		}
		if err := tx.RecordGitHubRunnerID(attemptID, int64(jit.RunnerID)); err != nil {
			return err
		}
		return tx.TransitionLease(lease.ID, store.LeaseProvisioning, store.LeaseRuntimeRegistered)
	}); err != nil {
		log.Error("registering runner in state failed", "error", err)
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}

	// A repository-scoped binding leases an exclusive cache lane; the
	// lane is freed by lease teardown, so it survives a crash and is
	// reclaimed on the next reconciliation.
	var cacheMount capsule.CacheMount
	if b.cacheEnabled {
		loc, ok, err := s.cache.Acquire(prepCtx, b.ref.CanonicalURL, b.generation, lease.ID, b.maxLanes)
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

	recorder := s.leases.Recorder(ctx, lease.ID)
	s.advanceAttempt(ctx, attemptID, store.AttemptLeased, store.AttemptPreparing)
	sandbox, err := s.netSandbox.forLaunch(prepCtx)
	if err != nil {
		log.Error("network sandbox refresh failed", "error", err)
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	prepared, err := s.caps.Prepare(prepCtx, capsule.Spec{
		LeaseID:      lease.ID,
		InstanceID:   s.store.InstanceID(),
		AttemptID:    attemptID,
		TargetID:     assignment.TargetID(b.target.ID),
		TierID:       assignment.TierID(b.tier.ID),
		CapsuleImage: b.capsuleImage,
		JITConfig:    jit.Encoded,
		Resources:    b.tier.Resources,
		Cache:        cacheMount,
		Sandbox:      sandbox,
		CgroupDriver: s.cgroupDriver,
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
			s.leases.HoldAttempt(ctx, lease.ID, store.ReviewReasonIncompatibleCapsule)
		}
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	// Recorded after the preparation exists — evidence names what
	// happened, never what was about to be attempted.
	if err := s.leases.RecordEvidence(ctx, lease.ID, store.EvidenceRuntimePrepared); err != nil {
		log.Error("cannot record that the runtime is prepared", "error", err)
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	s.advanceAttempt(ctx, attemptID, store.AttemptPreparing, store.AttemptPrepared)
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
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
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
		s.recoverCapsuleFailure(ctx, b, lease.ID, assignment.ObservedNeverStarted)
		return
	}
	// The authorization is durable immediately before the one effect
	// that can begin execution, so the ambiguous window is exactly one
	// Docker request wide and a crash inside it is classified by
	// inspecting the runtime, never by guessing.
	if err := s.leases.RecordEvidence(ctx, lease.ID, store.EvidenceStartAuthorized); err != nil {
		log.Error("cannot record the start authorization", "error", err)
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	if startErr := s.caps.Start(prepCtx, prepared); startErr != nil {
		// A failed Start call does not prove no start: the request may
		// have taken effect before the error. Classify from the daemon
		// now, before any cleanup can destroy the answer.
		obs, ierr := s.inspectExecution(ctx, prepared)
		if ierr != nil {
			log.Error("start outcome cannot be observed", "start_error", startErr, "inspect_error", ierr)
		}
		startObs = obs
		switch obs {
		case assignment.ObservedRunning:
			log.Warn("start reported an error but the runtime is running; continuing", "error", startErr)
		case assignment.ObservedExited:
			log.Warn("start reported an error but the runtime ran and exited", "error", startErr)
			if err := s.leases.RecordEvidence(ctx, lease.ID, store.EvidenceExitObserved); err != nil {
				log.Error("cannot record the observed exit", "error", err)
			}
			s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
			return
		default:
			report, unproven := startFailureReport(obs)
			if unproven {
				log.Error(report, "observation", string(obs), "error", startErr)
			} else {
				log.Info(report, "observation", string(obs), "error", startErr)
			}
			s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
			return
		}
	}
	if err := s.leases.RecordEvidence(ctx, lease.ID, store.EvidenceRunningObserved); err != nil {
		log.Error("cannot record that the runner is running", "error", err)
	}
	s.advanceAttempt(ctx, attemptID, store.AttemptStarting, store.AttemptRunning)
	if err := s.leases.Transition(ctx, lease.ID, store.LeaseRuntimeRegistered, store.LeaseWorkloadRunning); err != nil {
		log.Error("transition to running failed", "error", err)
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	runnerContainer := prepared.RuntimeID
	log.Info("capsule running", "runner", jit.RunnerName, "container", docker.ShortID(string(runnerContainer)))

	// From here the tier's ceiling governs, and only here: this is the
	// wait for work the provider owns, and the ceiling is the backstop
	// for a capsule that stops reporting rather than a limit on the job.
	// An adopted capsule spends what its lease has left of the same
	// budget, so a restart neither extends nor restarts it.
	waitCtx, cancelWait := context.WithTimeout(ctx, remainingCeiling(b.tier, lease.CreatedAt))
	defer cancelWait()
	exit, err := s.wait.WaitExit(waitCtx, string(runnerContainer))
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
			s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
			return
		}
		if err := s.leases.RecordEvidence(ctx, lease.ID, store.EvidenceExitObserved); err != nil {
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
		s.recoverCapsuleFailure(ctx, b, lease.ID, startObs)
		return
	}
	if exit != 0 {
		tail, _ := s.wait.TailLogs(ctx, string(runnerContainer), 40)
		log.Warn("runner exited non-zero", "exit", exit, "log_tail", tail)
	} else {
		log.Info("runner exited cleanly")
	}

	if err := s.leases.Release(ctx, lease.ID, store.LeaseWorkloadRunning); err != nil {
		log.Error("cleanup failed; lease quarantined", "error", err)
		return
	}
	log.Info("lease released")
}

// releaseCreditIfDone returns the binding's admission credit only when the lease has
// reached its terminal state. A quarantined or otherwise stuck lease
// keeps consuming capacity until reconciliation or cleanup resolves it.
func (s *Controller) releaseCreditIfDone(b *binding, leaseID assignment.LeaseID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var lease store.Lease
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		lease, err = tx.LeaseByID(leaseID)
		return err
	}); err != nil {
		// The lease cannot be read, so its disposition is unknown; holding
		// the credit is the safe reading of an unknown.
		s.log.Error("cannot confirm lease disposition; holding its credit", "lease", leaseID, "error", err)
		return
	}
	if !lease.State.Terminal() {
		s.log.Warn("lease did not reach released; its credit stays held",
			"lease", leaseID, "state", string(lease.State))
		return
	}
	s.alloc.Release(b.key)
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
func (s *Controller) recoverCapsuleFailure(ctx context.Context, b *binding, leaseID assignment.LeaseID,
	startObs assignment.ExecutionObservation) error {

	ctx, cancel := context.WithTimeout(ctx, recoveryBudget)
	defer cancel()
	bindingKey := "(unconfigured)"
	if b != nil {
		bindingKey = string(b.key)
	}
	log := s.log.With("binding", bindingKey, "lease", leaseID)

	// After the drain window, recovery belongs to the successor. The
	// failures that arrive here now are the shutdown itself — the client
	// and store closing under a live wait — and acting on them would
	// rewrite a running lease to cleaning, handing the next start a
	// failure to dismantle instead of a capsule to adopt. A recovery
	// already past this point runs to completion: its transitions are
	// durable, and the successor resumes whatever it left.
	if s.abandoning.Load() {
		log.Warn("drain window elapsed; the lease is left as it is for the next start to recover")
		return nil
	}

	was, err := s.leases.ToCleaning(ctx, leaseID)
	if err != nil {
		log.Error("failure transition failed", "error", err)
		return err
	}

	// The runner id lives in the adapter's metadata for the attempt this
	// lease serves; a binding of another provider has none, and zero
	// means there is nothing to deregister. Reaching for it is this
	// layer's job precisely because the lease machine must not know a
	// provider exists.
	var runnerGitHubID int64
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		runnerGitHubID, err = tx.GitHubRunnerID(was.AttemptID)
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
			if startObs == assignment.ObservedCreated {
				log.Warn("the provider says the job was handed over; the capsule's own account "+
					"of never having started it is not what settles this",
					"observation", string(startObs))
				startObs = assignment.ObservedRunning
			}

		default:
			log.Warn("removing registered runner", "runner", runnerGitHubID, "error", err)
		}
	}

	if err := s.leases.RemoveResources(ctx, leaseID); err != nil {
		log.Error("failure cleanup failed; lease quarantined", "error", err)
		s.leases.Quarantine(leaseID)
		return err
	}
	if err := s.leases.Finalize(ctx, leaseID, startObs); err != nil {
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
	}
	return "", true
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
func (s *Controller) inspectExecution(ctx context.Context,
	prepared capsule.PreparedRuntime) (assignment.ExecutionObservation, error) {

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	return s.caps.InspectExecution(ctx, prepared)
}
