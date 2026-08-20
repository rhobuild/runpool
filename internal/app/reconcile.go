package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/store"
)

// defaultStrandedGrace is how long a live lease is left alone before the
// periodic pass may consider it ownerless. A test overrides it through
// the controller's own strandedGrace field.
//
// It is keyed on the last transition rather than on creation, so it also
// keeps the pass off a lease a goroutine is actively moving: every
// transition is a sign of an owner, and a lease that stops moving for
// this long has genuinely lost one. Long enough to cover the gap between
// a lease row committing and its owner registering in memory; short
// enough that a lease a crash really did strand is still recovered
// within one pass of noticing.
const defaultStrandedGrace = 2 * time.Minute

// reconcile aligns the books with the daemon at startup, across every
// binding. A lease whose capsule still runs is adopted, awaited
// and cleaned; every other live lease is released, and the resource
// sweep then removes any owned Docker object — registered or orphaned
// from a crash between create and record — that no adopted lease needs.
// Foreign objects are never touched: the instance label is the boundary.
func (s *Controller) reconcile(ctx context.Context) error {
	containers, err := s.objects.ListOwnedContainers(ctx, s.store.InstanceID())
	if err != nil {
		return err
	}

	var live []store.Lease
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		live, err = tx.LeasesInStates(store.LiveLeaseStates...)
		if err != nil {
			return err
		}
		// Release and disposition commit together, so this should always
		// be empty. It is swept anyway because an invariant nothing
		// checks is an assumption: a released lease whose attempt is
		// still open sits outside every working set, invisible to each
		// later startup. Pulling it back in by the attempt keeps the set
		// bounded by unresolved work rather than by every job ever run.
		stranded, err := tx.StrandedAttempts()
		if err != nil {
			return err
		}
		for _, a := range stranded {
			lease, err := tx.LeaseByAttempt(a.ID)
			if err != nil {
				return err
			}
			live = append(live, lease)
		}
		return nil
	}); err != nil {
		return err
	}
	if n := len(live); n > 0 {
		s.log.Info("reconciling interrupted leases", "count", n)
	}

	runnerByLease := make(map[assignment.LeaseID]docker.OwnedContainer, len(containers))
	for _, c := range containers {
		if c.Role == capsule.RoleCapsule {
			runnerByLease[c.LeaseID] = c
		}
	}

	adopted := make(map[assignment.LeaseID]bool)
	for _, lease := range live {
		b := s.byBinding[lease.BindingID] // may be nil if the target was removed
		// Adoption means "this lease is still executing; wait it out".
		// A lease already past draining is not executing — it is being
		// unwound, and adopting it would run WalkToRunning and then a
		// release guarded on `workload_running`, which conflicts and
		// returns before any cleanup. The lease would then hold its credit
		// and its privileged container forever, because being marked
		// adopted also exempts it from the orphan sweep.
		if runner, ok := runnerByLease[lease.ID]; ok && runner.Running && b != nil && adoptable(lease.State) {
			s.log.Info("adopting running capsule", "binding", b.key, "lease", lease.ID)
			s.reportAdoption(b, s.alloc.Adopt(b.key))
			s.adopt(b, lease, runner.ID)
			adopted[lease.ID] = true
			continue
		}
		// Every nonterminal lease holds a credit until it is resolved, even
		// with no runner left: its resources may still exist, and the
		// resolution below can end in quarantine, which keeps consuming
		// capacity until cleanup succeeds.
		if b != nil {
			s.reportAdoption(b, s.alloc.Adopt(b.key))
			defer s.releaseCreditIfDone(b, lease.ID)
		}
		runner, hasRunner := runnerByLease[lease.ID]
		s.resolveInterrupted(ctx, b, lease, runner, hasRunner)
	}

	// Nothing else runs yet at this point, so the set cannot move under
	// the enumeration; it is passed as a reader so one sweep serves both
	// callers.
	return s.sweepOrphans(ctx, func() (map[assignment.LeaseID]bool, error) { return adopted, nil })
}

// reportAdoption says when a restart has left the instance holding more
// than its budget. Adoption never refuses — the capsule is running and its
// resources are committed — so the books simply record what is true, and
// the pool stays over its limit until those leases release. It converges
// on its own; what it must not do is converge silently, because until it
// does the advertised total exceeds parallelism and no other line explains
// why.
//
// Both limits are named when both exist, because either can be the one
// breached: a tier inside its own parallelism can still put the instance
// past the limit every tier shares. With independent tiers the instance
// figure is left out — the report's fallback is a sum, not a limit anyone
// configured, and printing it beside a warning invites reading it as one.
func (s *Controller) reportAdoption(b *binding, overBudget bool) {
	if !overBudget {
		return
	}
	fields := []any{"binding", b.key, "tier", b.tier.ID, "tier_parallelism", b.tier.Parallelism}
	if report := s.alloc.CapacityReport(); report.Global {
		fields = append(fields,
			"instance_active", report.Active, "instance_parallelism", report.Parallelism)
	}
	s.log.Warn("adopted past the budget; capacity stays over its limit until these leases release",
		fields...)
}

// resolveInterrupted brings one lease with no adoptable runner to a
// terminal state and disposes of its attempt. Cleanup depends on where
// the crash left the lease — transitions valid from `provisioning` are
// invalid from `cleaning` — while the disposition depends only on what
// can be proven about execution:
//
//   - still setting up (reserved through workload_running) or already
//     failed/quarantined: release the lease, then dispose;
//   - already unwinding (draining, cleaning): resume the release, then
//     dispose;
//   - already released with its attempt still open: nothing to clean,
//     only the disposition runs.
func (s *Controller) resolveInterrupted(ctx context.Context, b *binding, lease store.Lease, runner docker.OwnedContainer, hasRunner bool) {
	// Recovery bookkeeping runs detached from the caller's context.
	// Reconciliation happens at startup, where a shutdown can cancel
	// mid-pass, and releasing a lease while failing to requeue its
	// attempt leaves that attempt attached to a lease that no longer
	// exists — invisible to every query, retried by nothing. The release
	// already survived cancellation; the requeue must too, or the pair
	// is not a recovery at all.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptedLeaseBudget)
	defer cancel()

	evidence, err := s.leases.EvidenceOf(ctx, lease)
	if err != nil {
		s.log.Error("cannot read the attempt's evidence; leaving the lease for the next pass",
			"lease", lease.ID, "error", err)
		return
	}
	log := s.log.With("lease", lease.ID,
		"state", string(lease.State), "evidence", string(evidence))

	// The observation is taken before any cleanup: the container is the
	// only proof of whether an authorized start took effect, and the
	// release below destroys it.
	obs := assignment.ObservedAbsent
	if evidence == store.EvidenceStartAuthorized && hasRunner {
		var err error
		obs, err = s.inspectExecution(ctx, capsule.PreparedRuntime{RuntimeID: assignment.RuntimeID(runner.ID)})
		if err != nil {
			log.Error("cannot observe the runtime of an ambiguous start", "error", err)
		}
	}

	// An exited runtime observed here refines the evidence before any
	// path destroys the container that proves it: the finalizing
	// transaction then settles from evidence alone. The write is the
	// whole point — nothing below reads the local copy, because every
	// disposition re-reads the row inside its own transaction.
	if obs == assignment.ObservedExited && evidence == store.EvidenceStartAuthorized {
		if err := s.store.Tx(ctx, func(tx *store.Tx) error {
			return tx.RecordEvidence(lease.AttemptID, store.EvidenceExitObserved)
		}); err != nil {
			log.Error("cannot record the observed exit", "error", err)
		}
	}

	switch lease.State {
	case store.LeaseDraining, store.LeaseCleaning:
		// Resume the interrupted release: external cleanup, then the
		// finalizing transaction disposes of the attempt atomically.
		log.Info("resuming an interrupted release")
		if err := s.leases.FinishCleaning(ctx, lease, obs); err != nil {
			log.Error("resuming the release failed; the lease stays quarantined", "error", err)
			s.leases.Quarantine(lease.ID)
		}

	case store.LeaseReleased:
		// The invariant sweep found a released lease whose attempt is
		// still open. Release and disposition commit together, so this
		// should be unreachable; reaching it means something violated
		// that, and the disposition is the only thing left to run.
		log.Warn("disposing of an attempt left open by a released lease")
		s.leases.DisposeStranded(ctx, lease, obs)

	case store.LeaseReserved, store.LeaseProvisioning,
		store.LeaseRuntimeRegistered, store.LeaseWorkloadRunning,
		store.LeaseFailed, store.LeaseQuarantined:
		// recoverCapsuleFailure finishes with the same finalizing
		// transaction, so release and disposition cannot come apart.
		log.Info("releasing an interrupted lease")
		if err := s.recoverCapsuleFailure(ctx, b, lease.ID, obs); err != nil {
			log.Error("recovery could not resolve the lease; reconciliation will retry", "error", err)
		}

	default:
		log.Warn("no recovery defined for this state; leaving it for an operator")
	}
}

// recoveryContext detaches unwinding from whatever bounded the step that
// failed.
//
// A step's context expiring is the failure the step's bound exists to
// produce, so it is the likeliest reason to be unwinding at all — and a
// recovery handed the context that just expired releases nothing. The
// capsule, its network and its volume stay behind, and the lease keeps
// the admission credit they were admitted on, which is capacity the host
// never gets back short of a restart.
//
// It keeps a bound of its own, because an unwind that hangs is a
// goroutine the drain waits out at shutdown.
func recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recoveryBudget)
}

func (s *Controller) adopt(b *binding, lease store.Lease, runnerContainer string) {
	// Claimed here rather than inside the goroutine, so no pass can see the
	// lease as ownerless in the gap before it is scheduled.
	s.claimLease(lease.ID)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.releaseLease(lease.ID)
		defer s.releaseCreditIfDone(b, lease.ID)
		// What is left of this lease's ceiling, not a fresh one: a capsule
		// that stopped reporting is exactly what the ceiling bounds, and
		// restarting the clock on every controller restart would let it
		// hold its credit indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(),
			remainingCeiling(b.tier, lease.CreatedAt))
		defer cancel()
		// Through the same seam the ordinary launch awaits its runner:
		// an adopted capsule is a capsule, and one of the two paths
		// reaching around it is how they come to behave differently.
		exit, err := s.wait.WaitExit(ctx, runnerContainer)
		done, endRecovery := recoveryContext(ctx)
		defer endRecovery()
		if err != nil {
			s.log.Error("adopted capsule wait failed", "lease", lease.ID, "error", err)
			if err := s.recoverCapsuleFailure(done, b, lease.ID, ""); err != nil {
				s.log.Error("adopted capsule could not be resolved; reconciliation will retry",
					"lease", lease.ID, "error", err)
			}
			return
		}
		// The same reading the launch path gives its own capsule: an
		// adopted capsule that stopped on the reserved code never handed
		// the job over, and discarding the status here settled it as a
		// run that finished.
		if obs := capsule.ClassifyExit(int(exit)); obs != assignment.ObservedExited {
			s.log.Warn("the adopted capsule reports the runner never started; the attempt "+
				"returns to the queue unless the provider says otherwise",
				"lease", lease.ID, "exit", exit)
			if err := s.recoverCapsuleFailure(done, b, lease.ID, obs); err != nil {
				s.log.Error("adopted capsule could not be resolved; reconciliation will retry",
					"lease", lease.ID, "error", err)
			}
			return
		}
		// The adopted capsule was awaited to its exit. Recording the
		// observation is what lets the finalizing transaction settle the
		// attempt from evidence, like any other completed run.
		if err := s.leases.RecordEvidence(done, lease.ID, store.EvidenceExitObserved); err != nil {
			s.log.Error("cannot record the adopted capsule's exit", "lease", lease.ID, "error", err)
		}
		if err := s.leases.WalkToRunning(done, lease); err != nil {
			s.log.Error("adopted capsule state repair failed", "lease", lease.ID, "error", err)
		}
		if err := s.leases.Release(done, lease.ID, store.LeaseWorkloadRunning); err != nil {
			s.log.Error("adopted capsule cleanup failed", "lease", lease.ID, "error", err)
			return
		}
		s.log.Info("adopted capsule released", "binding", b.key, "lease", lease.ID)
	}()
}

// sweepOrphans removes every owned Docker resource whose lease is not
// among the adopted ones — released leases' registered resources and any
// object stranded by a crash between creation and recording — in
// dependency order: containers, then networks, then volumes.
//
// Four kinds of failure, four answers. Reading which leases are live is
// one of them, and it is fatal for the same reason an unreadable
// inventory is: a pass that cannot say what is in use has proved nothing
// about what is garbage.
//
// One object that will not die does not stop the controller. A single
// wedged container — a dind in uninterruptible sleep, a volume held by a
// stale mount — used to abort startup entirely with no retry, while the
// per-lease intent saga books exponential backoff for exactly the same
// work. Startup was strictly less resilient than steady state. Those
// failures are counted and reported instead: the object keeps its
// ownership labels, and sweepPeriodically runs this again on the
// reconcile interval — so "the next sweep" is a real event and not "the
// next restart".
//
// Listing is fatal, because a sweep that cannot see the daemon has
// proven nothing and must not report an empty inventory as a clean one.
//
// Forgetting the record is fatal too, and for a different reason: it
// fails only when the store is unreachable, and everything the rest of
// this pass would do needs the same store. Aborting loses nothing —
// startup fails loudly, and the periodic caller logs and comes back in
// under a minute.
//
// What that abort does not undo is the object already removed from the
// daemon whose record did not go with it. That intent row is beyond
// every retry there is: the sweep works from what the daemon still
// lists, the stranded-lease pass works from live leases and this one is
// released, and retention refuses a lease that still owns an intent —
// deliberately, so the leak stays visible in `status` rather than being
// quietly forgotten.
func (s *Controller) sweepOrphans(ctx context.Context, liveLeases func() (map[assignment.LeaseID]bool, error)) error {
	wedged := 0
	fail := func(kind, name string, err error) {
		wedged++
		s.log.Error("cannot sweep an orphan; it stays labelled for the next pass",
			"kind", kind, "id", name, "error", err)
	}

	// The daemon is enumerated before the live set is read, and all of it
	// before any of it is judged. An object exists only after the lease
	// that owns it committed, so an object seen now whose lease is absent
	// in the later read is genuinely ownerless. Read the other way round,
	// every lease that commits between the two reads owns objects the
	// sweep cannot account for: it force-removes a capsule whose job is
	// running and deletes the records that would have cleaned up after
	// it. Cache lane collection states the same order for the same
	// reason.
	containers, err := s.objects.ListOwnedContainers(ctx, s.store.InstanceID())
	if err != nil {
		return err
	}
	networks, err := s.objects.ListOwnedNetworks(ctx, s.store.InstanceID())
	if err != nil {
		return err
	}
	volumes, err := s.objects.ListOwnedVolumes(ctx, s.store.InstanceID())
	if err != nil {
		return err
	}
	keep, err := liveLeases()
	if err != nil {
		return err
	}

	for _, c := range containers {
		if keep[c.LeaseID] {
			continue
		}
		if c.HelperInFlight() {
			continue
		}
		s.log.Info("sweeping orphan container", "name", c.Name, "lease", c.LeaseID)
		if err := s.objects.RemoveContainer(ctx, c.ID); err != nil {
			fail("container", c.Name, err)
			continue
		}
		// The durable record goes with the object it described. Removing
		// one without the other leaves the daemon and the books
		// disagreeing, and the foreign key then blocks the lease from
		// ever being purged.
		if err := s.leases.ForgetResource(ctx, c.LeaseID, c.ID); err != nil {
			return err
		}
	}
	for _, n := range networks {
		if keep[n.LeaseID] {
			continue
		}
		if n.InstanceInfrastructure() {
			continue
		}
		if err := s.objects.RemoveNetwork(ctx, n.ID); err != nil {
			fail("network", n.ID, err)
			continue
		}
		if err := s.leases.ForgetResource(ctx, n.LeaseID, n.ID); err != nil {
			return err
		}
	}
	for _, v := range volumes {
		if keep[v.LeaseID] {
			continue
		}
		if v.InstanceInfrastructure() {
			continue
		}
		if err := s.objects.RemoveVolume(ctx, v.ID); err != nil {
			fail("volume", v.ID, err)
			continue
		}
		if err := s.leases.ForgetResource(ctx, v.LeaseID, v.ID); err != nil {
			return err
		}
	}
	if wedged > 0 {
		s.log.Warn("some orphans could not be swept; they keep their labels and are retried next pass",
			"count", wedged)
	}
	return nil
}

// periodicReconcile retries what the serving paths gave up on, without
// waiting for a restart. Its working set is every live lease no goroutine
// owns — quarantine is the common case, but it is not the only way an
// owner stops: a failed finalization parks a lease in `cleaning`, and a
// failed transition leaves one wherever it was. Those are just as
// ownerless, and they hold an admission credit until something resolves
// them, so narrowing the set to quarantine leaked capacity until restart.
// Ownership, not state, is what makes a lease this pass's business.
// Each retry honours the backoff booked on the lease's intents, so one
// wedged object paces its own attempts instead of hammering the daemon;
// and each pass is bounded, because an unbounded sweep is how a
// reconciler becomes the load it was meant to absorb.
func (s *Controller) periodicReconcile(ctx context.Context) {
	const base = 45 * time.Second
	for {
		wait := base + time.Duration(rand.Int64N(int64(base/3)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.retryStranded(ctx)
		s.sweepPeriodically(ctx)
		s.prunePeriodically(ctx)
	}
}

// prunePeriodically forgets the record of finished leases past the
// configured window. Bounded per pass like its neighbours: the books grow
// with every job the host runs, and a sweep that tried to catch up in one
// go would be the load it exists to prevent.
//
// It writes one audit row per pass, not one per lease. RunGC records
// each eviction because a volume is an object an operator can look at; a
// pass that forgets five thousand rows would flood the one table nothing
// ever prunes.
func (s *Controller) prunePeriodically(ctx context.Context) {
	if s.leaseHistory <= 0 {
		return // keep every lease record; the operator asked for it
	}
	const perPass = 500
	before := time.Now().Add(-s.leaseHistory)
	var pruned int
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		pruned, err = tx.PruneLeaseHistory(before, perPass)
		if err != nil || pruned == 0 {
			return err
		}
		return tx.RecordAudit("retention", "retention_prune", "capsule_leases",
			fmt.Sprintf("pruned=%d older_than=%s", pruned, s.leaseHistory))
	}); err != nil {
		s.log.Error("retention pass failed", "error", err)
		return
	}
	if pruned > 0 {
		s.log.Info("pruned lease history", "leases", pruned, "older_than", s.leaseHistory.String())
	}
}

// sweepPeriodically is the retry the startup sweep's failures depend on.
// sweepOrphans logs and continues rather than aborting the controller, but
// it used to run only at startup — so "the next pass finds it again" meant
// "the next restart", and a wedged privileged container leaked for the life
// of the process behind a warning.
//
// Everything with a live lease is kept, which is stricter than the startup
// sweep needs to be and is the right default here: a lease still in the
// machine is either being driven by a goroutine or about to be resolved by
// retryStranded, and in both cases its objects are that owner's to remove.
// What is left is a genuine orphan — an object whose lease is released or
// gone entirely.
func (s *Controller) sweepPeriodically(ctx context.Context) {
	if err := s.sweepOrphans(ctx, func() (map[assignment.LeaseID]bool, error) {
		var live []store.Lease
		if err := s.store.Tx(ctx, func(tx *store.Tx) error {
			var err error
			live, err = tx.LeasesInStates(store.LiveLeaseStates...)
			return err
		}); err != nil {
			// Named, because the caller now has one message for every
			// fatal cause and two of them are store failures.
			return nil, fmt.Errorf("list live leases: %w", err)
		}
		keep := make(map[assignment.LeaseID]bool, len(live))
		for _, lease := range live {
			keep[lease.ID] = true
		}
		return keep, nil
	}); err != nil {
		s.log.Error("periodic sweep failed", "error", err)
	}
}

// retryStranded drives one bounded pass over live leases nobody owns.
// ToCleaning begins from the lease's actual state, so the same recovery
// resolves a lease from anywhere in the machine; what must not happen is
// touching one a goroutine is still driving, which is what the claim is
// for. A lease that fails to claim is not stranded — it has an owner.
func (s *Controller) retryStranded(ctx context.Context) {
	var live []store.Lease
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		live, err = tx.LeasesInStates(store.LiveLeaseStates...)
		return err
	}); err != nil {
		s.log.Error("periodic reconcile cannot list live leases", "error", err)
		return
	}
	const perPass = 8
	stranded, retried, converged := 0, 0, 0
	for _, lease := range live {
		if retried >= perPass {
			break
		}
		if time.Since(lease.UpdatedAt) < s.strandedAfter() {
			// Recently touched, so its owner is either running or about
			// to register. The claim that marks a lease owned is taken
			// in memory just after the row commits, and a pass that ran
			// inside that gap would find the lease unclaimed, call it
			// stranded, and tear down a capsule that is starting — after
			// which both owners conclude the lease is done and release
			// its credit twice. Elapsed time is the only evidence of the
			// gap that exists here, so it is what the pass waits on.
			continue
		}
		if !s.claimLease(lease.ID) {
			continue // a goroutine is driving it; not this pass's business
		}
		stranded++
		func() {
			// Deferred so a panic cannot leak the claim. A leaked claim
			// makes the lease invisible to every later pass, which is the
			// leak this whole mechanism exists to prevent.
			defer s.releaseLease(lease.ID)
			s.resolveStranded(ctx, lease, &retried, &converged)
		}()
	}
	if retried > 0 {
		s.log.Info("periodic reconcile pass",
			"stranded", stranded, "retried", retried, "converged", converged)
	}
}

// resolveStranded is one claimed lease's recovery, split out so the claim
// is released on every path out.
func (s *Controller) resolveStranded(ctx context.Context, lease store.Lease, retried, converged *int) {
	due, err := s.leases.IntentsDue(ctx, lease.ID)
	if err != nil {
		s.log.Error("periodic reconcile cannot read intents", "lease", lease.ID, "error", err)
		return
	}
	if !due {
		return // its backoff has not elapsed; let it breathe
	}
	*retried++
	b := s.byBinding[lease.BindingID] // may be nil if the target was removed
	if err := s.recoverCapsuleFailure(ctx, b, lease.ID, ""); err != nil {
		s.log.Warn("stranded lease still unresolved",
			"lease", lease.ID, "state", string(lease.State), "error", err)
		return
	}
	*converged++
	if b != nil {
		s.releaseCreditIfDone(b, lease.ID)
	}
}

// adoptable reports whether a lease found with a running capsule can be
// awaited rather than unwound. Everything up to `workload_running` is a
// job still in flight; `draining` onward is a release already under way,
// and resolveInterrupted is what knows how to finish one.
func adoptable(state store.LeaseState) bool {
	switch state {
	case store.LeaseReserved, store.LeaseProvisioning,
		store.LeaseRuntimeRegistered, store.LeaseWorkloadRunning:
		return true
	default:
		return false
	}
}
