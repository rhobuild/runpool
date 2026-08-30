package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

// The Start fault matrix: every way the launch orchestration can fail,
// driven through the real runCapsule with the capsule, registry and
// waiter seams faulted. The invariant across the whole table is that no
// combination produces a false "executed" — an attempt settles only
// when execution was observed, returns to ready only when a start
// provably never took effect, and is held for a person whenever neither
// can be shown.

type fakeCapsule struct {
	prepareErr error
	startErr   error
	obs        assignment.ExecutionObservation
	obsErr     error
	// spec is what the controller asked for, which is the only place the
	// image decisions are observable.
	spec capsule.Spec
	// onPrepare runs inside the preparation, which is the window a real
	// redelivery lands in: preparing a capsule takes minutes and the
	// attempt is out of the launching goroutine's sight for all of it.
	onPrepare func()
	// onInspect sees the context an observation is made under, which is
	// the only place its bound is observable.
	onInspect func(context.Context)
	// starts counts the one effect that can begin execution.
	starts int
}

func (f *fakeCapsule) Prepare(_ context.Context, spec capsule.Spec, rec capsule.ResourceRecorder) (capsule.PreparedRuntime, error) {
	f.spec = spec
	if f.onPrepare != nil {
		f.onPrepare()
	}
	if f.prepareErr != nil {
		return capsule.PreparedRuntime{}, f.prepareErr
	}
	// A successful preparation records its objects the way the real one
	// does — plan, creating, confirm — so the finalizing transaction has
	// real intents to verify and remove.
	id, err := rec.Plan("container", "capsule", "runpool-runner-fake")
	if err != nil {
		return capsule.PreparedRuntime{}, err
	}
	if err := rec.Creating(id); err != nil {
		return capsule.PreparedRuntime{}, err
	}
	if err := rec.Confirm(id, "fake-runner"); err != nil {
		return capsule.PreparedRuntime{}, err
	}
	return capsule.PreparedRuntime{RuntimeID: "fake-runner"}, nil
}

func (f *fakeCapsule) Start(context.Context, capsule.PreparedRuntime) error {
	f.starts++
	return f.startErr
}

func (f *fakeCapsule) InspectExecution(ctx context.Context, _ capsule.PreparedRuntime) (assignment.ExecutionObservation, error) {
	if f.onInspect != nil {
		f.onInspect(ctx)
	}
	return f.obs, f.obsErr
}

// nullProvider answers every provider method with its zero value. The
// scenario fakes embed it and override exactly the calls their scenario
// is about, so merging the provider seams did not force every fake to
// carry methods its test never exercises.
type nullProvider struct{}

func (nullProvider) EnsureScaleSet(context.Context, string, string, int, bool, func() error) (githubactions.ScaleSet, error) {
	return githubactions.ScaleSet{}, nil
}

func (nullProvider) GenerateJITConfig(context.Context, int, string, string) (githubactions.JITConfig, error) {
	return githubactions.JITConfig{}, nil
}

func (nullProvider) RemoveRunner(context.Context, int) error { return nil }

func (nullProvider) OpenSession(context.Context, int, string) (*githubactions.Session, error) {
	return nil, errors.New("this fake opens no sessions")
}

type fakeRegistry struct {
	nullProvider
	jitErr error
	// removeErr is what the provider answers when asked to deregister
	// the runner, which is the one question in this machine put to a
	// party that did not run the job.
	removeErr error
}

func (f *fakeRegistry) GenerateJITConfig(context.Context, int, string, string) (githubactions.JITConfig, error) {
	if f.jitErr != nil {
		return githubactions.JITConfig{}, f.jitErr
	}
	return githubactions.JITConfig{RunnerID: 5, RunnerName: "fake", Encoded: "fake-jit"}, nil
}

func (f *fakeRegistry) RemoveRunner(context.Context, int) error { return f.removeErr }

type fakeWaiter struct {
	exit    int64
	waitErr error
	// deadline captures the wait context's bound, which is where the
	// tier's ceiling — or what a lease has left of it — must arrive.
	deadline time.Time
}

func (f *fakeWaiter) WaitExit(ctx context.Context, _ string) (int64, error) {
	if d, ok := ctx.Deadline(); ok {
		f.deadline = d
	}
	return f.exit, f.waitErr
}
func (f *fakeWaiter) TailLogs(context.Context, string, int) (string, error) {
	return "", nil
}

// runFaulted claims one attempt and drives runCapsule synchronously
// against the given fakes.
func runFaulted(t *testing.T, h *harness, caps *fakeCapsule, reg *fakeRegistry, wait *fakeWaiter, workload assignment.SourceWorkloadKey) (store.Lease, string) {
	t.Helper()
	h.srv.executor.capsule = caps
	h.srv.executor.waiter = wait
	h.bind.gh = reg
	if err := h.deliver(demand(string(workload), "app", 60)); err != nil {
		t.Fatal(err)
	}
	lease, attemptID := leaseFor(t, h, workload)
	h.srv.executor.runCapsule(h.bind, lease)
	return lease, string(attemptID)
}

func TestStartFaultMatrix(t *testing.T) {
	boom := errors.New("injected failure")
	cases := []struct {
		name    string
		caps    *fakeCapsule
		reg     *fakeRegistry
		wait    *fakeWaiter
		want    store.AttemptState    // attempt state afterwards
		wantRes assignment.Resolution // attempt resolution, when settled
	}{
		{
			// The credential could not be minted: nothing was prepared,
			// nothing could have run.
			name: "jit generation fails",
			caps: &fakeCapsule{}, reg: &fakeRegistry{jitErr: boom}, wait: &fakeWaiter{},
			want: store.AttemptReady,
		},
		{
			// Preparation died building the capsule: by construction no
			// start was possible.
			name: "prepare fails",
			caps: &fakeCapsule{prepareErr: boom}, reg: &fakeRegistry{}, wait: &fakeWaiter{},
			want: store.AttemptReady,
		},
		{
			// Start errored and the daemon shows the container never left
			// created: the one provable requeue past the authorization.
			name: "start error, runtime proven inert",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedNeverStarted}, reg: &fakeRegistry{}, wait: &fakeWaiter{},
			want: store.AttemptReady,
		},
		{
			// The capsule's own account that it never started, against a
			// provider that still holds the runner busy with the job.
			// One of those two parties did not run the job, and it is not
			// the capsule -- whose account the job inside it can write.
			name: "capsule says it never started, provider holds the runner busy",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedCreated},
			reg:  &fakeRegistry{removeErr: githubactions.ErrJobStillRunning}, wait: &fakeWaiter{},
			want: store.AttemptSettled, wantRes: assignment.ResolutionStartedObserved,
		},
		{
			// The daemon's account, against the same answer. This one is
			// not the capsule's word and not the weaker of the two: the
			// container was never started, observed from outside the
			// machine the job runs in. The provable requeue stays.
			name: "daemon says it never started, provider holds the runner busy",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedNeverStarted},
			reg:  &fakeRegistry{removeErr: githubactions.ErrJobStillRunning}, wait: &fakeWaiter{},
			want: store.AttemptReady,
		},
		{
			// An outcome nobody could establish is still nobody's to
			// settle. The provider says a runner was busy, not that this
			// attempt's runner ran, so the hold stands.
			name: "runtime absent, provider holds the runner busy",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedAbsent},
			reg:  &fakeRegistry{removeErr: githubactions.ErrJobStillRunning}, wait: &fakeWaiter{},
			want: store.AttemptManualReview,
		},
		{
			// And only that answer. Any other failure to deregister says
			// nothing about who had the job.
			name: "capsule says it never started, deregistration fails some other way",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedCreated},
			reg:  &fakeRegistry{removeErr: boom}, wait: &fakeWaiter{},
			want: store.AttemptReady,
		},
		{
			// Start errored but the container ran and exited: the error
			// was noise, the execution was real.
			name: "start error, runtime ran and exited",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedExited}, reg: &fakeRegistry{}, wait: &fakeWaiter{},
			want: store.AttemptSettled, wantRes: assignment.ResolutionCompletedObserved,
		},
		{
			// Start errored but the container is running: continue as if
			// the start succeeded, await it, settle on its exit.
			name: "start error, runtime running",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedRunning}, reg: &fakeRegistry{}, wait: &fakeWaiter{exit: 0},
			want: store.AttemptSettled, wantRes: assignment.ResolutionCompletedObserved,
		},
		{
			// A clean start whose wait returns the code the supervisor
			// reserves for "the runner never owned the job". A clean wait
			// is not an execution: recording an observed exit here settles
			// an attempt that never ran as complete, and nothing requeues
			// it afterwards.
			name: "clean start, supervisor reports the runner never started",
			caps: &fakeCapsule{}, reg: &fakeRegistry{},
			wait: &fakeWaiter{exit: int64(capsule.SupervisorAbortedExitCode)},
			want: store.AttemptReady,
		},
		{
			// The same reserved code against a provider that still holds
			// the runner busy: the party that assigned the work outranks
			// the capsule's own account, on the wait path exactly as on
			// the failed-start path above.
			name: "clean start, reserved code, provider holds the runner busy",
			caps: &fakeCapsule{}, reg: &fakeRegistry{removeErr: githubactions.ErrJobStillRunning},
			wait: &fakeWaiter{exit: int64(capsule.SupervisorAbortedExitCode)},
			want: store.AttemptSettled, wantRes: assignment.ResolutionStartedObserved,
		},
		{
			// Start errored and the container is gone: nothing can be
			// proven in either direction, so a person decides.
			name: "start error, runtime absent",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedAbsent}, reg: &fakeRegistry{}, wait: &fakeWaiter{},
			want: store.AttemptManualReview,
		},
		{
			// Start errored and the daemon cannot be asked: same ruling.
			name: "start error, daemon unavailable",
			caps: &fakeCapsule{startErr: boom, obs: assignment.ObservedUnavailable, obsErr: boom}, reg: &fakeRegistry{}, wait: &fakeWaiter{},
			want: store.AttemptManualReview,
		},
		{
			// The runner started and the daemon was lost mid-run: running
			// was observed, so the attempt settles as exactly that.
			name: "wait fails after a clean start",
			caps: &fakeCapsule{}, reg: &fakeRegistry{}, wait: &fakeWaiter{waitErr: boom},
			want: store.AttemptSettled, wantRes: assignment.ResolutionStartedObserved,
		},
		{
			// A non-zero exit is the job's business, not the machine's:
			// the runner ran and completed.
			name: "runner exits non-zero",
			caps: &fakeCapsule{}, reg: &fakeRegistry{}, wait: &fakeWaiter{exit: 7},
			want: store.AttemptSettled, wantRes: assignment.ResolutionCompletedObserved,
		},
		{
			name: "happy path",
			caps: &fakeCapsule{}, reg: &fakeRegistry{}, wait: &fakeWaiter{exit: 0},
			want: store.AttemptSettled, wantRes: assignment.ResolutionCompletedObserved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 1)
			lease, attemptID := runFaulted(t, h, tc.caps, tc.reg, tc.wait, "job-fault")

			got := attemptState(t, h, assignment.AttemptID(attemptID))
			if got.State != tc.want {
				t.Errorf("attempt = %s; want %s", got.State, tc.want)
			}
			if tc.wantRes != "" && got.Resolution != tc.wantRes {
				t.Errorf("resolution = %s; want %s", got.Resolution, tc.wantRes)
			}
			// No combination may leave the lease holding its credit: every
			// path ends terminal, whatever became of the attempt.
			if final := reloadLease(t, h, lease.ID); final.State != store.LeaseReleased {
				t.Errorf("lease ended %s; want released", final.State)
			}
			// The invariant the atomic ending exists for: after any
			// normal path, no attempt is left open under a finished
			// lease. The reconciler's sweep should never find one.
			h.inStore(func(s *store.Tx) error {
				stranded, err := s.StrandedAttempts()
				if err != nil {
					return err
				}
				if len(stranded) != 0 {
					t.Errorf("stranded attempts = %d after a normal path; release and "+
						"disposition came apart", len(stranded))
				}
				return nil
			})
		})
	}
}

// The operator's two decisions over held work, with their audit trail.
// Retry is only offered because the operator verified externally that
// the workload never executed; settle is the safe reading when it
// cannot be ruled out.
func TestOperatorResolvesHeldAttempts(t *testing.T) {
	boom := errors.New("injected failure")
	for _, decision := range []string{"retry", "settle"} {
		t.Run(decision, func(t *testing.T) {
			h := newHarness(t, 1)
			// An unprovable start outcome puts the attempt in review.
			_, attemptID := runFaulted(t, h,
				&fakeCapsule{startErr: boom, obs: assignment.ObservedAbsent},
				&fakeRegistry{}, &fakeWaiter{}, "job-held")
			if got := attemptState(t, h, assignment.AttemptID(attemptID)); got.State != store.AttemptManualReview {
				t.Fatalf("attempt = %s; want manual_review", got.State)
			}

			h.inStore(func(s *store.Tx) error {
				if decision == "retry" {
					return s.ResolveReviewToReady(assignment.AttemptID(attemptID), "provider shows the job never started", "matias")
				}
				return s.ResolveReviewToSettled(assignment.AttemptID(attemptID), assignment.ResolutionMayHaveExecuted,
					"cannot rule out execution", "matias")
			})

			got := attemptState(t, h, assignment.AttemptID(attemptID))
			switch decision {
			case "retry":
				if got.State != store.AttemptReady {
					t.Errorf("attempt = %s; want ready", got.State)
				}
				if len(h.ready()) != 1 {
					t.Error("a retried attempt is not servable")
				}
			case "settle":
				if got.State != store.AttemptSettled || got.Resolution != assignment.ResolutionMayHaveExecuted {
					t.Errorf("attempt = %s/%s; want settled/%s", got.State, got.Resolution, assignment.ResolutionMayHaveExecuted)
				}
			}
			if got.ReviewedBy != "matias" {
				t.Errorf("reviewed_by = %q; every resolution names its actor", got.ReviewedBy)
			}
			// The decision and its reason are in the lifecycle, where an
			// audit reads them back.
			var sawResolution bool
			h.inStore(func(s *store.Tx) error {
				events, err := s.Events(assignment.AttemptID(attemptID))
				if err != nil {
					return err
				}
				for _, ev := range events {
					if ev.Kind == "operator_resolved" && ev.Detail != "{}" {
						sawResolution = true
					}
				}
				return nil
			})
			if !sawResolution {
				t.Error("no operator_resolved event with detail; the audit trail is missing")
			}
		})
	}
}

// faultyRemover fails removals until healed — a wedged daemon the
// periodic reconciler must outlast.
type faultyRemover struct {
	healed bool
}

func (f *faultyRemover) fail() error {
	if f.healed {
		return nil
	}
	return errors.New("daemon wedged")
}
func (f *faultyRemover) RemoveOwnedContainer(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return f.fail()
}
func (f *faultyRemover) RemoveOwnedNetwork(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return f.fail()
}
func (f *faultyRemover) RemoveOwnedVolume(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	return f.fail()
}

// The periodic reconciler converges a quarantined lease without a
// restart: the wedged removal parks the lease and books backoff on the
// intent; the pass respects the backoff while it lasts, and once the
// daemon heals and the window elapses, one pass drives the whole ending
// — removal, release, disposition.
func TestPeriodicReconcileConvergesQuarantine(t *testing.T) {
	h := newHarness(t, 1)
	remover := &faultyRemover{}
	h.useRemover(remover)
	if err := h.deliver(demand("job-wedged", "app", 80)); err != nil {
		t.Fatal(err)
	}
	lease, attempt := leaseFor(t, h, "job-wedged")
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		id, err := tx.PlanResource(lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-wedge")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(id, "wedge-1")
	}); err != nil {
		t.Fatal(err)
	}

	// The removal fails: the lease parks in quarantine with backoff
	// booked on the intent, and the attempt stays leased — unresolved,
	// visible, waiting.
	if err := h.srv.executor.recoverCapsuleFailure(t.Context(), h.bind, lease.ID, assignment.NoObservation); err == nil {
		t.Fatal("recoverCapsuleFailure with a wedged daemon succeeded")
	}
	if got := reloadLease(t, h, lease.ID); got.State != store.LeaseQuarantined {
		t.Fatalf("lease = %s; want quarantined", got.State)
	}

	// The backoff has not elapsed: a periodic pass must not retry yet.
	h.srv.reconciler.retryStranded(t.Context())
	if got := reloadLease(t, h, lease.ID); got.State != store.LeaseQuarantined {
		t.Fatalf("a pass inside the backoff window touched the lease: %s", got.State)
	}

	// The daemon heals and the backoff elapses.
	remover.healed = true
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		intents, err := tx.Resources(lease.ID)
		if err != nil {
			return err
		}
		return tx.RecordResourceError(intents[0].ID, errors.New("previous failure"), time.Now().Add(-time.Second))
	}); err != nil {
		t.Fatal(err)
	}

	h.srv.reconciler.retryStranded(t.Context())

	if got := reloadLease(t, h, lease.ID); got.State != store.LeaseReleased {
		t.Errorf("lease = %s after the periodic pass; want released", got.State)
	}
	if got := attemptState(t, h, attempt); got.State != store.AttemptReady {
		t.Errorf("attempt = %s; want ready — nothing was ever authorized", got.State)
	}
}

// A provider cancellation closes unstarted work and nothing else: the
// ready attempt cancels, while an attempt already serving is left to
// its own lifecycle — a cancellation aimed at old work must never touch
// a live capsule.
func TestRemoteCancellationClosesOnlyReadyWork(t *testing.T) {
	h := newHarness(t, 2)
	if err := h.deliver(demand("job-idle", "app", 70)); err != nil {
		t.Fatal(err)
	}
	if err := h.deliver(demand("job-busy", "app", 71)); err != nil {
		t.Fatal(err)
	}
	_, busyAttempt := leaseFor(t, h, "job-busy") // now leased, not ready

	cancelled := &githubactions.Message{ID: 900, Completed: []assignment.WorkloadLifecycleEvent{
		{SourceWorkloadKey: "job-idle", Result: "canceled", Canceled: true},
		{SourceWorkloadKey: "job-busy", Result: "canceled", Canceled: true},
	}}
	s := h.srv.supervisor
	s.recordLifecycleEvents(t.Context(), h.bind, cancelled)

	idle := h.ready()
	if len(idle) != 0 {
		t.Errorf("a cancelled ready attempt is still servable: %+v", idle)
	}
	if got := attemptState(t, h, busyAttempt); got.State != store.AttemptLeased {
		t.Errorf("a serving attempt was touched by a remote cancellation: %s", got.State)
	}
}

// TestPeriodicReconcileConvergesAStrandedCleaningLease is the credit-leak
// guard. Quarantine is not the only way an owner stops: a failed
// finalization leaves the lease in `cleaning` with no goroutine driving it.
// The old pass listed quarantined leases only, so such a lease held its
// admission credit until the process restarted — the tier permanently
// admitting one less than its parallelism.
func TestPeriodicReconcileConvergesAStrandedCleaningLease(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-stranded", "app", 80)); err != nil {
		t.Fatal(err)
	}
	lease, attempt := leaseFor(t, h, "job-stranded")

	// Park the lease in cleaning with nobody driving it, which is where a
	// failed Finalize leaves it, and take the credit its owner held.
	if _, err := h.srv.executor.leases.ToCleaning(t.Context(), lease.ID); err != nil {
		t.Fatal(err)
	}
	if got := reloadLease(t, h, lease.ID).State; got != store.LeaseCleaning {
		t.Fatalf("lease = %s; want cleaning", got)
	}
	h.srv.alloc.Adopt(h.bind.key)
	if h.srv.alloc.TryReserve(h.bind.key) {
		t.Fatal("the stranded lease should be holding the tier's only credit")
	}

	h.srv.reconciler.retryStranded(t.Context())

	if got := reloadLease(t, h, lease.ID).State; got != store.LeaseReleased {
		t.Errorf("stranded cleaning lease = %s after a periodic pass; want released", got)
	}
	if !h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("the stranded lease's admission credit was never returned")
	}
	if got := attemptState(t, h, attempt); got.State == store.AttemptLeased {
		t.Error("the attempt was left leased to a lease that no longer exists")
	}
}

// A lease a goroutine is still driving is not stranded, and the periodic
// pass must not touch it — two owners each concluding it is done would
// release the same admission credit twice and oversubscribe the host.
func TestPeriodicReconcileSkipsOwnedLeases(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-owned", "app", 80)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-owned")

	if !h.srv.ownership.claim(lease.ID) {
		t.Fatal("the lease should be claimable")
	}
	defer h.srv.ownership.release(lease.ID)

	before := reloadLease(t, h, lease.ID).State
	h.srv.reconciler.retryStranded(t.Context())
	if got := reloadLease(t, h, lease.ID).State; got != before {
		t.Errorf("a claimed lease moved %s -> %s; the pass must leave owned leases alone", before, got)
	}
}

// TestPeriodicReconcileSpareAJobBeingLaunched closes the window the
// ownership claim opened. createLease commits a lease in `reserved` — a
// live state the periodic pass now lists — and the goroutine that drives
// it starts afterwards. If the claim were taken inside that goroutine, a
// pass landing in between would find the lease ownerless and drive it
// through recoverCapsuleFailure: the capsule, its network and its volume
// destroyed under a job that was starting, and its admission credit
// released twice.
func TestPeriodicReconcileSpareAJobBeingLaunched(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-launching", "app", 80)); err != nil {
		t.Fatal(err)
	}
	h.serve()

	launched := h.launchedAttempts()
	if len(launched) != 1 {
		t.Fatalf("launched %v; want exactly one attempt", launched)
	}
	h.mu.Lock()
	lease := h.leases[launched[0]]
	h.mu.Unlock()
	if got := reloadLease(t, h, lease.ID).State; got != store.LeaseReserved {
		t.Fatalf("lease = %s; the harness launch leaves it reserved", got)
	}

	h.srv.reconciler.retryStranded(t.Context())

	if got := reloadLease(t, h, lease.ID).State; got != store.LeaseReserved {
		t.Errorf("the periodic pass drove a lease being launched: %s -> %s",
			store.LeaseReserved, got)
	}
	if h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("the launching lease's admission credit was released out from under it")
	}
}

// TestTheWaitInheritsWhatTheLeaseHasLeft: the ceiling bounds how long
// this instance waits for one capsule, and a lease adopted after a
// restart has already spent part of it. remainingCeiling holds that
// rule; this pins the wiring — that runCapsule's wait actually receives
// the remainder, because a wait built from the tier's full ceiling
// would hand every restart a fresh budget and the wedge the ceiling
// exists to bound would be extended by each one.
func TestTheWaitInheritsWhatTheLeaseHasLeft(t *testing.T) {
	h := newHarness(t, 1)
	h.bind.tier.JobTimeout = ptrDuration(2 * time.Hour)
	caps := &fakeCapsule{}
	wait := &fakeWaiter{}
	h.srv.executor.capsule = caps
	h.srv.executor.waiter = wait
	h.bind.gh = &fakeRegistry{}
	if err := h.deliver(demand("job-adopted", "app", 61)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-adopted")

	// An adopted lease is one whose clock started in a previous process:
	// age it ninety minutes, then drive it.
	aged := lease
	aged.CreatedAt = time.Now().Add(-90 * time.Minute)
	before := time.Now()
	h.srv.executor.runCapsule(h.bind, aged)

	if wait.deadline.IsZero() {
		t.Fatal("the wait carried no deadline; the ceiling never reached it")
	}
	left := wait.deadline.Sub(before)
	if left > 40*time.Minute {
		t.Fatalf("the wait was given %s; a ninety-minute-old lease on a two-hour ceiling has ~30m left, "+
			"so the restart handed it a fresh budget", left.Round(time.Minute))
	}
	if left < 20*time.Minute {
		t.Fatalf("the wait was given %s; want roughly the thirty minutes the lease has left", left.Round(time.Minute))
	}
}

func ptrDuration(d time.Duration) *config.Duration {
	cd := config.Duration(d)
	return &cd
}

// TestARedeliveryNeverSupersedesItsOwnAttempts: resolving an
// open-attempt conflict must not close the attempts the same delivery
// just inserted.
//
// RecordDelivery inserts the workloads it reaches before the conflicting
// one, so a caller that sweeps every workload the delivery carries finds
// those first. Superseding one and retrying leaves the retry reporting
// it as already inserted — the row is there, superseded — and the
// workload is never served, under a delivery that did land and a
// message that is acknowledged.
func TestARedeliveryNeverSupersedesItsOwnAttempts(t *testing.T) {
	h := newHarness(t, 2)

	// A predecessor for the second workload only. The order is the
	// point: the new delivery inserts job-a, then conflicts on job-b,
	// so by the time the conflict is resolved this delivery already owns
	// an open attempt of its own for a workload the sweep will visit.
	if err := h.deliver(demand("job-b", "app", 2)); err != nil {
		t.Fatal(err)
	}

	if err := h.deliver(demand("job-a", "app", 3), demand("job-b", "app", 4)); err != nil {
		t.Fatalf("the redelivery did not persist: %v", err)
	}

	ready := h.ready()
	if len(ready) != 2 {
		var got []string
		for _, a := range ready {
			got = append(got, string(a.SourceWorkloadKey))
		}
		t.Fatalf("servable workloads = %v; want both job-a and job-b under the new delivery", got)
	}
	seen := map[string]bool{}
	for _, a := range ready {
		seen[string(a.SourceWorkloadKey)] = true
	}
	if !seen["job-a"] || !seen["job-b"] {
		t.Errorf("servable workloads = %v; want both", seen)
	}
}

// TestAFreshLeaseIsNotStranded closes the window the in-memory claim
// cannot cover. The claim is taken just after the lease row commits, so
// a pass landing in between finds the lease unclaimed — and tearing down
// a capsule that is starting ends with both owners releasing the same
// admission credit.
func TestAFreshLeaseIsNotStranded(t *testing.T) {
	h := newHarness(t, 1)
	// The production grace, against a lease this test just created: the
	// harness shortens it, and shortening it is what would make this
	// assert nothing.
	h.srv.reconciler.strandedGrace = defaultStrandedGrace
	if err := h.deliver(demand("job-fresh", "app", 90)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-fresh")
	h.srv.alloc.Adopt(h.bind.key)

	before := reloadLease(t, h, lease.ID).State
	h.srv.reconciler.retryStranded(t.Context())

	if got := reloadLease(t, h, lease.ID).State; got != before {
		t.Errorf("a lease committed moments ago moved %s -> %s; its owner had not registered yet", before, got)
	}
	if h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("the pass released the credit of a lease whose owner was still starting")
	}
}

// TestASupersededAttemptIsNeverStarted: superseding an attempt that a
// goroutine is already preparing must stop that goroutine before the
// start, not after it.
//
// Preparing a capsule takes minutes, and the attempt is out of the
// launching goroutine's sight for all of them: the walk logs a lost
// transition and carries on. So the edge into `starting` is made
// authoritative — a compare-and-swap that matches no row means something
// else resolved this attempt, and the successor is already serving the
// workload. Starting anyway runs it twice.
func TestASupersededAttemptIsNeverStarted(t *testing.T) {
	h := newHarness(t, 1)
	caps := &fakeCapsule{}
	h.srv.executor.capsule = caps
	h.srv.executor.waiter = &fakeWaiter{}
	h.bind.gh = &fakeRegistry{}
	if err := h.deliver(demand("job-raced", "app", 42)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-raced")

	// The redelivery lands while the capsule is being prepared.
	caps.onPrepare = func() {
		h.inStore(func(tx *store.Tx) error {
			return tx.SupersedeOpenAttempt(h.bind.bindingID, "job-raced",
				assignment.ResolutionSuperseded, 0)
		})
	}

	h.srv.executor.runCapsule(h.bind, lease)

	if caps.starts != 0 {
		t.Errorf("the capsule was started %d time(s) for an attempt its successor owns", caps.starts)
	}
	if got := reloadLease(t, h, lease.ID).State; got != store.LeaseReleased {
		t.Errorf("lease = %s; want released — a superseded attempt must not pin the lease serving it", got)
	}
	if !h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("the lease's admission credit was never returned")
	}
}

// blockingRemover holds every removal until its context ends — a daemon
// that accepted the call and is answering nothing.
type blockingRemover struct{}

func (blockingRemover) block(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (b blockingRemover) RemoveOwnedContainer(ctx context.Context, _ string, _ assignment.InstanceID, _ assignment.LeaseID) error {
	return b.block(ctx)
}
func (b blockingRemover) RemoveOwnedNetwork(ctx context.Context, _ string, _ assignment.InstanceID, _ assignment.LeaseID) error {
	return b.block(ctx)
}
func (b blockingRemover) RemoveOwnedVolume(ctx context.Context, _ string, _ assignment.InstanceID, _ assignment.LeaseID) error {
	return b.block(ctx)
}

// TestTheReconcilerStopsRecoveringWhenTheShutdownBegins: the periodic
// pass's recovery ends when the loop's context does.
//
// Its work is resumable by definition — the next pass, or the next
// start, finds the lease exactly where this left it — so there is
// nothing to protect by detaching it, and a recovery detached from
// everything runs its own two-minute budget against a wedged daemon
// while the platform's grace period expires around it. The shutdown
// bound the deployment is sized against has to be one the process
// actually keeps.
func TestTheReconcilerStopsRecoveringWhenTheShutdownBegins(t *testing.T) {
	h := newHarness(t, 1)
	h.useRemover(blockingRemover{})
	if err := h.deliver(demand("job-blocked", "app", 81)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-blocked")
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		id, err := tx.PlanResource(lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-blocked")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(id, "blocked-1")
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	h.srv.reconciler.retryStranded(ctx)
	elapsed := time.Since(start)

	if elapsed > LoopStopBudget {
		t.Errorf("the pass held the shutdown for %s against a %s budget; a recovery detached "+
			"from the loop outlives the grace period the deployment is sized for",
			elapsed.Round(time.Second), LoopStopBudget)
	}
}

// TestALostWalkEdgeDoesNotAbortTheLaunch: an observability write that
// did not land must not cost a prepared capsule and a serving.
//
// The walk into `preparing` and into `prepared` is best-effort by
// design: it is written outside the transaction that matters, and a
// failure is logged rather than retried, because its only reader is a
// person. The edge into `starting` is the opposite — it is the last
// point before an effect that can begin execution. Making that edge
// require the exact predecessor tied an authoritative decision to a
// best-effort one, so a transient store error during preparation tore
// down a capsule that was ready to run and burnt one of the attempt's
// servings.
//
// What the edge must still refuse is an attempt somebody else resolved;
// TestASupersededAttemptIsNeverStarted is that half.
func TestALostWalkEdgeDoesNotAbortTheLaunch(t *testing.T) {
	h := newHarness(t, 1)
	caps := &fakeCapsule{}
	h.srv.executor.capsule = caps
	h.srv.executor.waiter = &fakeWaiter{}
	h.bind.gh = &fakeRegistry{}
	if err := h.deliver(demand("job-lost-edge", "app", 42)); err != nil {
		t.Fatal(err)
	}
	lease, attemptID := leaseFor(t, h, "job-lost-edge")

	// `leased -> preparing` is written just before Prepare is called, so
	// putting the attempt back to `leased` from inside it is what that
	// write failing leaves behind. The other lost edge, `preparing ->
	// prepared`, leaves the attempt at `preparing`; the store's own table
	// test decides both, and this one takes the further of the two.
	caps.onPrepare = func() {
		h.inStore(func(tx *store.Tx) error {
			return tx.Advance(attemptID, store.AttemptPreparing, store.AttemptLeased)
		})
	}

	h.srv.executor.runCapsule(h.bind, lease)

	if caps.starts != 1 {
		t.Errorf("the capsule was started %d time(s); want 1 — the attempt was still this serving's, "+
			"only one state further back than the walk managed to record", caps.starts)
	}
	if got := reloadLease(t, h, lease.ID).State; got != store.LeaseReleased {
		t.Errorf("lease = %s; want released", got)
	}
	if !h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("the lease's admission credit was never returned")
	}
}

// TestAnObservationIsAlwaysBounded: observing a runtime carries a
// deadline of its own.
//
// The observation is a `docker exec` into the capsule, and an exec ends
// when its context ends and not before — nothing in the Docker API
// cancels one. Both callers hold something open while they wait: this
// one is the launch goroutine the drain counts, and the other is the
// reconciliation pass every later pass queues behind. Handed a context
// with no deadline, a daemon that stopped answering turns a slow
// shutdown into one that does not finish.
func TestAnObservationIsAlwaysBounded(t *testing.T) {
	h := newHarness(t, 1)
	var (
		deadline    time.Time
		hasDeadline bool
		observed    bool
	)
	caps := &fakeCapsule{
		startErr: errors.New("the daemon refused the start"),
		obs:      assignment.ObservedCreated,
		onInspect: func(ctx context.Context) {
			observed = true
			deadline, hasDeadline = ctx.Deadline()
		},
	}
	h.srv.executor.capsule = caps
	h.srv.executor.waiter = &fakeWaiter{}
	h.bind.gh = &fakeRegistry{}
	if err := h.deliver(demand("job-observed", "app", 42)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-observed")

	h.srv.executor.runCapsule(h.bind, lease)

	if !observed {
		t.Fatal("the start failed and nothing observed the runtime; this test asserted nothing")
	}
	if !hasDeadline {
		t.Fatal("the observation ran on a context with no deadline; a daemon that stops answering never returns it")
	}
	if left := time.Until(deadline); left <= 0 || left > inspectTimeout {
		t.Errorf("the observation had %v left; want a positive bound no larger than %v", left, inspectTimeout)
	}
}

// TestRecoveryOutlivesTheContextThatBoundedTheWait: unwinding a lease
// does not inherit the deadline whose expiry is the reason to unwind.
//
// The tier's ceiling bounds the wait for a capsule that has stopped
// reporting. That is the failure it exists to produce, so it is the
// likeliest reason to be recovering at all — and a recovery handed the
// context that just expired removes nothing. The capsule, its network
// and its volume stay, and the lease keeps the admission credit they
// were admitted on, which is capacity no later pass recovers.
func TestRecoveryOutlivesTheContextThatBoundedTheWait(t *testing.T) {
	ceiling, expire := context.WithCancel(context.Background())
	unwind, done := recoveryContext(ceiling)
	defer done()

	expire()

	if err := unwind.Err(); err != nil {
		t.Fatalf("the recovery context ended with the wait's: %v; nothing it unwinds would be released", err)
	}
	if _, ok := unwind.Deadline(); !ok {
		t.Error("the recovery context has no deadline; an unwind against a wedged daemon would hold the pass forever")
	}
}

// ctxRemover records the context its removals run under. That context is
// the only place the bound on unwinding is observable from outside.
type ctxRemover struct {
	seen        bool
	hasDeadline bool
	deadline    time.Time
}

func (r *ctxRemover) note(ctx context.Context) error {
	r.seen = true
	r.deadline, r.hasDeadline = ctx.Deadline()
	return nil
}
func (r *ctxRemover) RemoveOwnedContainer(ctx context.Context, _ string, _ assignment.InstanceID, _ assignment.LeaseID) error {
	return r.note(ctx)
}
func (r *ctxRemover) RemoveOwnedNetwork(ctx context.Context, _ string, _ assignment.InstanceID, _ assignment.LeaseID) error {
	return r.note(ctx)
}
func (r *ctxRemover) RemoveOwnedVolume(ctx context.Context, _ string, _ assignment.InstanceID, _ assignment.LeaseID) error {
	return r.note(ctx)
}

// TestAnAdoptedLeaseUnwindsOnItsOwnBudget: a wait that fails on an
// adopted capsule unwinds it, and does so under a bound.
//
// What this does not prove, said plainly because the obvious reading is
// wrong: it does not show that the unwind is detached from the context
// that bounded the wait. recoverCapsuleFailure derives its own
// recoveryBudget from whatever it is given, so the deadline seen here is
// two minutes either way. The detachment only shows once the ceiling has
// actually expired, and remainingCeiling floors at a minute of grace, so
// no unit test can produce that without changing a production timing.
// recoveryContext carries its own test for that half.
//
// What it does hold: the adopted path still unwinds, it removes the
// objects the capsule owned, and it runs under a deadline rather than
// none.
func TestAnAdoptedLeaseUnwindsOnItsOwnBudget(t *testing.T) {
	h := newHarness(t, 1)
	rec := &ctxRemover{}
	h.useRemover(rec)
	h.srv.executor.capsule = &fakeCapsule{}
	h.srv.executor.waiter = &fakeWaiter{waitErr: errors.New("the capsule stopped reporting")}
	h.bind.gh = &fakeRegistry{}
	if err := h.deliver(demand("job-adopted", "app", 42)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-adopted")

	// A capsule left behind by a previous process owns objects, which is
	// what makes the unwind have anything to do.
	recorder := h.srv.executor.leases.Recorder(t.Context(), lease.ID)
	intent, err := recorder.Plan("container", "capsule", "adopted-runner")
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Creating(intent); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Confirm(intent, "adopted-runner"); err != nil {
		t.Fatal(err)
	}

	h.srv.reconciler.adopt(h.bind, lease, "adopted-runner")
	<-h.srv.ownership.wait()

	if !rec.seen {
		t.Fatal("the adopted lease was unwound without removing anything; this test asserted nothing")
	}
	if !rec.hasDeadline {
		t.Fatal("the unwind ran on a context with no deadline")
	}
	if left := time.Until(rec.deadline); left <= 0 || left > recoveryBudget {
		t.Errorf("the unwind had %v left; want its own budget of at most %v. "+
			"Anything longer is the tier's ceiling, which is the deadline that just failed",
			left, recoveryBudget)
	}
}

// TestEveryObservationHasAStartFailureReport: a failed start reports
// what its observation means, and every observation means something.
//
// A switch says nothing about a value nobody added a case for. It falls
// into whatever branch is last, which reads as a decision and is not
// one — that is how the daemon's own account of a container it never
// started came to be reported as an outcome needing an operator, at the
// level an operator is paged on, for the ordinary case of a start that
// failed and left nothing behind.
func TestEveryObservationHasAStartFailureReport(t *testing.T) {
	for _, obs := range assignment.AllExecutionObservations {
		report, unproven := startFailureReport(obs)
		if report == "" {
			t.Errorf("observation %q has no report, so a failed start carrying it is logged "+
				"as an outcome nobody could establish and paged on", obs)
			continue
		}
		// The daemon's own account is the one this exists to keep apart
		// from an outcome nobody established.
		if obs == assignment.ObservedNeverStarted && unproven {
			t.Error("the daemon reporting a container it never started is not an unprovable " +
				"outcome; it is the clearest answer there is")
		}
	}
	// A value this package does not know makes no claim about what it
	// means -- and still says so, because the caller logs the report as
	// the message. An empty one is an operator page with nothing to
	// filter, alert or search on.
	report, unproven := startFailureReport("something-nobody-declared")
	if !unproven {
		t.Error("an observation this package does not know was reported as an established outcome")
	}
	if !strings.Contains(report, "does not declare") {
		t.Errorf("an undeclared observation reported %q; it has to say that it is undeclared "+
			"rather than name a meaning, and it has to say something", report)
	}
}

// TestAProofSurvivesThePeriodicPass is the reachable sequence, driven
// through the real recovery and the real periodic pass.
//
// A capsule reports that the runner never owned the job, so the attempt
// must return to the queue even though the capsule had already reported
// itself up. Removal then fails against a wedged daemon and the lease
// parks in quarantine. The periodic pass that picks it up carries no
// observation at all — it holds no daemon inventory — so without the
// serving's own record it disposes of the attempt on evidence alone and
// settles a job that never ran. Nothing serves that workload again, and
// the books say it started.
func TestAProofSurvivesThePeriodicPass(t *testing.T) {
	h := newHarness(t, 1)
	remover := &faultyRemover{}
	h.useRemover(remover)
	if err := h.deliver(demand("job-proof", "app", 81)); err != nil {
		t.Fatal(err)
	}
	lease, attempt := leaseFor(t, h, "job-proof")
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		// The capsule reported itself up and the attempt is running:
		// this is what a settled disposition would rule on.
		if err := tx.RecordEvidence(attempt, store.EvidenceRunningObserved); err != nil {
			return err
		}
		if err := tx.Advance(attempt, store.AttemptLeased, store.AttemptStarting); err != nil {
			return err
		}
		if err := tx.Advance(attempt, store.AttemptStarting, store.AttemptRunning); err != nil {
			return err
		}
		id, err := tx.PlanResource(lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-proof")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(id, "proof-1")
	}); err != nil {
		t.Fatal(err)
	}

	// The pass that measured the proof cannot finish.
	if err := h.srv.executor.recoverCapsuleFailure(t.Context(), h.bind, lease.ID,
		assignment.ObservedCreated); err == nil {
		t.Fatal("recovery with a wedged daemon succeeded")
	}
	if got := reloadLease(t, h, lease.ID); got.StartObservation != assignment.ObservedCreated {
		t.Fatalf("the serving recorded %q; the proof has to outlive the pass that took it",
			got.StartObservation)
	}

	// The daemon heals and the backoff elapses.
	remover.healed = true
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		intents, err := tx.Resources(lease.ID)
		if err != nil {
			return err
		}
		return tx.RecordResourceError(intents[0].ID, errors.New("previous failure"), time.Now().Add(-time.Second))
	}); err != nil {
		t.Fatal(err)
	}

	h.srv.reconciler.retryStranded(t.Context())

	if got := reloadLease(t, h, lease.ID); got.State != store.LeaseReleased {
		t.Errorf("lease = %s after the periodic pass; want released", got.State)
	}
	if got := attemptState(t, h, attempt); got.State != store.AttemptReady {
		t.Errorf("attempt = %s; want ready — the capsule proved the runner never owned this job", got.State)
	}
}

// TestAbandonmentKeepsWhatThisServingMeasured: past the drain window the
// lease is left as it is for the successor to recover, and that is
// right -- but what this pass measured is not left with it unless it is
// written down. Nothing re-takes it: the successor's pass only inspects
// a runtime while the evidence is still start_authorized, and an
// ambiguous start reaches here past that. Discarded, the capsule's proof
// that the runner never owned the job is gone and the successor settles
// the attempt as one that ran.
func TestAbandonmentKeepsWhatThisServingMeasured(t *testing.T) {
	h := newHarness(t, 1)
	remover := &countingRemover{}
	h.useRemover(remover)
	if err := h.deliver(demand("job-drained", "app", 91)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-drained")
	before := reloadLease(t, h, lease.ID).State

	h.srv.ownership.abandonUnfinished()
	if err := h.srv.executor.recoverCapsuleFailure(t.Context(), h.bind, lease.ID,
		assignment.ObservedCreated); err != nil {
		t.Fatalf("abandoned recovery reported an error: %v", err)
	}
	if got := reloadLease(t, h, lease.ID).StartObservation; got != assignment.ObservedCreated {
		t.Errorf("the serving recorded %q past the drain window; nothing else will ever take that measurement", got)
	}
	// And the branch still does what it exists for.
	if got := reloadLease(t, h, lease.ID).State; got != before {
		t.Errorf("lease moved from %s to %s; abandonment leaves it as it is", before, got)
	}
	if n := remover.calls.Load(); n != 0 {
		t.Errorf("abandonment removed %d objects; it must dismantle nothing", n)
	}
}

// TestTheProviderOverrulesTheCapsuleAcrossARetry: the capsule's account
// of never having handed the job over is produced inside the machine
// running that job, and the provider's answer replaces it. That exchange
// happens after the recorded proof is read back, which is the whole
// reason the read is placed where it is: on a retry the capsule is gone
// and only the record carries its account, so a readback that ran later
// -- or a write that ran earlier -- would leave the provider with
// nothing to overrule and requeue a job it says was handed over.
func TestTheProviderOverrulesTheCapsuleAcrossARetry(t *testing.T) {
	h := newHarness(t, 1)
	remover := &faultyRemover{}
	h.useRemover(remover)
	reg := &fakeRegistry{removeErr: errors.New("provider unreachable")}
	h.bind.gh = reg
	if err := h.deliver(demand("job-overruled", "app", 92)); err != nil {
		t.Fatal(err)
	}
	lease, attempt := leaseFor(t, h, "job-overruled")
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		if err := tx.RecordGitHubRunnerID(attempt, 4242); err != nil {
			return err
		}
		if err := tx.RecordEvidence(attempt, store.EvidenceRunningObserved); err != nil {
			return err
		}
		if err := tx.Advance(attempt, store.AttemptLeased, store.AttemptStarting); err != nil {
			return err
		}
		if err := tx.Advance(attempt, store.AttemptStarting, store.AttemptRunning); err != nil {
			return err
		}
		id, err := tx.PlanResource(lease.ID, store.ResourceContainer, store.ResourceRoleCapsule, "runpool-runner-overruled")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(id, "overruled-1")
	}); err != nil {
		t.Fatal(err)
	}

	// The capsule says the runner never owned the job. The provider
	// cannot be reached this pass, and removal fails, so the lease parks.
	if err := h.srv.executor.recoverCapsuleFailure(t.Context(), h.bind, lease.ID,
		assignment.ObservedCreated); err == nil {
		t.Fatal("recovery with a wedged daemon succeeded")
	}
	if got := reloadLease(t, h, lease.ID).StartObservation; got != assignment.ObservedCreated {
		t.Fatalf("the serving recorded %q; the capsule's account is what the provider has to overrule", got)
	}

	// The retry measures nothing of its own -- the capsule is being
	// removed -- and the provider now says it still holds the runner. This
	// pass fails too, so what it learned has to be written down here or it
	// is gone: the overrule reaches the disposition as a parameter only on
	// a pass that completes.
	reg.removeErr = githubactions.ErrJobStillRunning
	if err := h.srv.executor.recoverCapsuleFailure(t.Context(), h.bind, lease.ID,
		assignment.NoObservation); err == nil {
		t.Fatal("the second pass succeeded against a wedged daemon")
	}
	if got := reloadLease(t, h, lease.ID).StartObservation; got != assignment.ObservedRunning {
		t.Fatalf("the serving recorded %q after the provider overruled the capsule; the record has to be "+
			"written after that exchange, not before it", got)
	}

	// A third pass, with nothing measured and the provider unreachable
	// again: the disposition comes from the record alone.
	remover.healed = true
	reg.removeErr = errors.New("provider unreachable")
	if err := h.srv.executor.recoverCapsuleFailure(t.Context(), h.bind, lease.ID,
		assignment.NoObservation); err != nil {
		t.Fatalf("the third pass failed: %v", err)
	}
	got := attemptState(t, h, attempt)
	if got.State != store.AttemptSettled || got.Resolution != assignment.ResolutionStartedObserved {
		t.Errorf("attempt = %s/%s; the party that assigned the work says it was handed over, "+
			"so it is not returned to the queue", got.State, got.Resolution)
	}
}

// TestTheFirstTransitionUnwindsLikeEveryOther: every failure in a
// capsule's launch walks the lease back through the failure path, which
// is what returns its admission credit and its attempt to the queue. The
// first one -- the transition out of reserved -- returned bare instead,
// so a transient store error there left the lease reserved and its
// credit held until the stranded grace elapsed and a periodic pass
// noticed. Minutes of one tier's capacity, on the one step where that
// was the cost.
func TestTheFirstTransitionUnwindsLikeEveryOther(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-unwound", "app", 94)); err != nil {
		t.Fatal(err)
	}
	lease, attempt := leaseFor(t, h, "job-unwound")

	// The lease is already past reserved, so the transition this launch
	// opens with conflicts -- the natural shape of that step failing.
	if err := h.srv.executor.leases.Transition(t.Context(), lease.ID,
		store.LeaseReserved, store.LeaseProvisioning); err != nil {
		t.Fatal(err)
	}

	h.srv.executor.runCapsule(h.bind, lease)

	if got := reloadLease(t, h, lease.ID); got.State != store.LeaseReleased {
		t.Errorf("lease = %s after a launch that could not start; want released — "+
			"nothing unwound it and its credit is held until the stranded grace elapses", got.State)
	}
	if got := attemptState(t, h, attempt); got.State != store.AttemptReady {
		t.Errorf("attempt = %s; want ready — nothing was ever prepared for it", got.State)
	}
}
