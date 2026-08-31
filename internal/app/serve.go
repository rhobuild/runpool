// Package app owns the controller's process lifecycle: wiring
// configuration, state, GitHub and Docker into the serve loops, and
// shutting the whole thing down in order. Business rules live in the
// packages it composes.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/doctor"
	"github.com/rhobuild/runpool/internal/engine/docker"
	"github.com/rhobuild/runpool/internal/lease"
	"github.com/rhobuild/runpool/internal/netsandbox"
	"github.com/rhobuild/runpool/internal/store"
)

const (
	workFolder = "_work"
	// DrainTimeout bounds the wait for capsule goroutines at shutdown. A
	// capsule running a job outlives any window worth waiting — that is
	// what adoption on the next start is for — so this is sized to be
	// spent, and to leave the deployment's stop grace period enough room
	// afterwards to close every message session. A window that outlasts
	// that grace period is never reached: the platform sends SIGKILL
	// first, the sessions stay open, and the next start waits each one
	// out as a conflict.
	//
	// Exported with SessionCloseBudget because their sum is a deployment
	// contract: the reference deployment's stop grace period is sized
	// against it, and the consistency suite reads both sides.
	DrainTimeout = 60 * time.Second
	// SessionCloseBudget bounds closing every binding's message session
	// at shutdown — all of them together, under one budget. One budget
	// and not one per binding: the deployment's stop grace period has to
	// hold drain plus this, and a bound that grew with the binding count
	// would outgrow any grace period an operator chose.
	SessionCloseBudget = 15 * time.Second
	// LoopStopBudget is how long waiting for the serve loops may take
	// once their context is cancelled. Each loop stops on that context;
	// what can outlive one is a write deliberately detached so it would
	// survive cancellation, and the longest of those is the interrupted
	// lease recovery at thirty seconds.
	//
	// Unlike its two neighbours this is a claim rather than a timer.
	// Racing the wait against one would abandon a loop that is still
	// running, which falsifies what closing the sessions afterwards
	// assumes — that each binding's session field is quiescent — and
	// puts a data race on it. So the wait stays unbounded and this
	// number states what the loops must cost; the reconciler test is
	// what keeps that true.
	LoopStopBudget = 45 * time.Second
	// ShutdownBudget is the whole of it, and the number the deployment's
	// stop grace period is sized against — not a subset of its terms. A
	// grace period shorter than this ends in SIGKILL partway through,
	// which leaves every message session open for the next start to wait
	// out as a conflict.
	ShutdownBudget = LoopStopBudget + DrainTimeout + SessionCloseBudget
	// recoveryBudget bounds unwinding one lease after a failure.
	recoveryBudget = 2 * time.Minute
	pollBackoff    = 5 * time.Second

	// capsulePrepTimeout bounds getting a capsule to the point of running:
	// minting a credential, acquiring a lane, building the sandbox,
	// pulling and booting the image, and handing over the credential.
	// That is this instance's own work against its own daemon, so it
	// either finishes in a knowable time or something here is wrong. It
	// is deliberately generous — a cold image pull is the slow step — and
	// deliberately not the tier's ceiling, which answers a different
	// question about a different party.
	capsulePrepTimeout = 15 * time.Minute

	// interruptedLeaseBudget bounds resolving one lease left behind by a
	// previous process: reading its evidence, observing its runtime if
	// the start was authorized, and unwinding it.
	interruptedLeaseBudget = 30 * time.Second

	// inspectTimeout bounds one observation of a runtime. It reaches the
	// capsule through a `docker exec`, and an exec ends when its context
	// does and not before: a daemon that has stopped answering parks the
	// caller for as long as it is given, and neither caller can proceed
	// past it — one is a launch goroutine the drain counts, the other is
	// startup.
	//
	// It is deliberately a fraction of interruptedLeaseBudget rather than
	// equal to it. A bound equal to the budget it sits inside can never
	// take effect, and worse, an observation that spends the whole budget
	// leaves the unwinding that follows it a context with nothing left. A
	// container inspect and one supervisor exec do not need ten seconds;
	// a daemon that does is one the observation should give up on.
	inspectTimeout = 10 * time.Second

	// receiveFailuresBeforeReopen is how many consecutive polls may fail
	// before the binding stops trusting its session. The upstream client
	// refreshes an expired session itself and leaves the handle dead when
	// that refresh fails, so a run of identical failures is the shape a
	// dead session takes. Three keeps a transient 5xx from costing a
	// session while still recovering within a minute.
	receiveFailuresBeforeReopen = 3

	// A session left by a crashed controller lingers in the broker until
	// it expires by inactivity; the successor already holds the local
	// lock, so the loop waits it out rather than giving up on the binding.
	sessionConflictBackoff = 10 * time.Second
	// sessionConflictGrace is how long a broker may hold a predecessor's
	// session before the wait stops being ordinary. The broker expires a
	// session by inactivity well inside this, so past it the binding is
	// taking no new work for a reason that will not resolve itself. It
	// keeps serving what it already holds, and the reason it records
	// changes to say both of those things -- the report holds one string
	// per binding, and waiting one out and being stuck behind one have to
	// read differently in it.
	sessionConflictGrace = 5 * time.Minute
)

type Options struct {
	Version      string
	CapsuleImage string
	StateDir     string
	Environ      func(string) string
}

// Serve runs the controller until ctx is cancelled, then drains.
func Serve(ctx context.Context, cfg *config.Config, opts Options) error {
	log := newLogger(cfg.Observability.Log)
	log.Info("host topology selected", "topology", cfg.Host.Topology,
		"reserve_cpu", cfg.Host.Reserve.CPU.String(),
		"reserve_memory", cfg.Host.Reserve.Memory.String(),
		"reserve_swap", cfg.Host.Reserve.Swap.String(),
		"reserve_free_disk", cfg.Host.Reserve.FreeDisk.String())

	// The network profile decides the capsule topology. The restricted
	// profile builds its sandbox below, once the daemon is connected;
	// unsafe-open must keep being an explicit, logged choice.
	if cfg.Network.Profile == config.NetworkProfileUnsafeOpen {
		log.Warn("network profile is unsafe-open-egress: capsules reach whatever this host reaches, including the LAN and host services")
	}

	capsuleImg, err := CapsuleImage(opts.Environ, opts.CapsuleImage)
	if err != nil {
		return err
	}
	// The build inputs stay verified even though the controller no
	// longer runs them directly: the capsule image is built from exactly
	// these digests, and refusing to start on a broken lock is what
	// keeps that reviewable.
	if _, _, err := buildInputImages(); err != nil {
		return err
	}
	log.Info("capsule image", "image", capsuleImg)

	lock, err := store.TryAcquire(opts.StateDir)
	if err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			return fmt.Errorf("%w; only one controller may use a state directory", err)
		}
		return err
	}
	defer lock.Release()

	st, err := store.Open(opts.StateDir, cfg.Scheduling.RetryBudget)
	if err != nil {
		return err
	}
	defer st.Close()
	log = log.With("instance", st.InstanceID()[:8])

	dock, err := docker.New(ctx)
	if err != nil {
		return err
	}
	defer dock.Close()
	dock.OnCleanupError(func(name string, err error) {
		// The ownership labels guarantee a later sweep finds it, but a
		// helper that outlives its task is worth seeing now.
		log.Warn("a setup helper could not be removed", "container", name, "error", err)
	})

	// The daemon's own facts, read once. The cgroup driver decides the
	// form of every lease's parent cgroup, and the daemon rejects the
	// wrong form — so it is read from the host rather than assumed.
	hostInfo, err := dock.Info(ctx)
	if err != nil {
		return fmt.Errorf("read daemon facts: %w", err)
	}

	// The host contract is checked before anything is created on GitHub:
	// a host that cannot honour it must fail here, not midway through a
	// job. Live credential checks are left to `runpool doctor`, so a
	// transient API failure never blocks a restart with running capsules.
	report := doctor.Run(ctx, doctor.Options{
		Config:   cfg,
		Docker:   dock,
		StateDir: opts.StateDir,
		Environ:  opts.Environ,
	})
	for _, res := range report.Results {
		switch res.Status {
		case doctor.Fail:
			log.Error("doctor", "check", res.Name, "detail", res.Detail, "fix", res.Fix)
		case doctor.Warn:
			log.Warn("doctor", "check", res.Name, "detail", res.Detail, "fix", res.Fix)
		default:
			log.Debug("doctor", "check", res.Name, "detail", res.Detail)
		}
	}
	if !report.OK() {
		return errors.New("host does not meet the runtime contract; run `runpool doctor` for the full report")
	}

	// The restricted profile's sandbox: uplink and deny-set snapshot,
	// assembled before any binding exists. A discovery failure fails
	// serve closed — a capsule must never launch with a partial policy.
	var netSandbox *netsandbox.Manager
	if cfg.Network.Profile == config.NetworkProfilePublicInternetOnly {
		netSandbox, err = netsandbox.New(ctx, dock, st.InstanceID(), capsuleImg, cfg, log)
		if err != nil {
			return fmt.Errorf("network sandbox: %w", err)
		}
	}

	// Cache lanes are daemon-side named volumes, so there is nothing to
	// resolve about how this controller is deployed: the same volume is
	// the same object from a container or from the host, which is
	// independent of where the controller process is mounted.
	cacheMgr := cache.New(st, dock, st.InstanceID())

	s := &Controller{
		log:        log,
		store:      st,
		alloc:      newAllocator(cfg),
		netSandbox: netSandbox,
		ownership:  newLeaseOwnership(),
	}
	s.executor = &leaseExecutor{
		log:          log,
		store:        st,
		capsule:      capsule.NewLauncher(dock, capsuleImg),
		waiter:       dock,
		leases:       lease.NewManager(st, dock, log),
		cache:        cacheMgr,
		allocator:    s.alloc,
		ownership:    s.ownership,
		network:      netSandbox,
		cgroupDriver: hostInfo.CgroupDriver,
	}
	s.disk = newDiskMonitor(cfg, log, st, dock, cacheMgr, s.alloc, capsuleImg)
	// Before any binding serves: capacity stays closed until a current
	// measurement is durable. A failed probe does not prevent adopting
	// running capsules; the background monitor keeps retrying it.
	if err := s.disk.initialize(ctx); err != nil {
		return fmt.Errorf("initialize disk pressure: %w", err)
	}
	s.scheduler = &attemptScheduler{
		log:         log,
		store:       st,
		allocator:   s.alloc,
		ownership:   s.ownership,
		pressure:    s.currentPressure,
		createLease: s.executor.createLease,
		launch:      s.executor.runCapsule,
	}
	s.supervisor = &bindingSupervisor{
		log:                 log,
		store:               st,
		allocator:           s.alloc,
		scheduler:           s.scheduler,
		shippedCapsuleImage: capsuleImg,
		byBinding:           map[assignment.BindingID]*binding{},
	}
	if err := s.supervisor.buildBindings(ctx, cfg, opts.Environ); err != nil {
		return err
	}
	s.reconciler = &reconciler{
		log:          log,
		store:        st,
		objects:      dock,
		allocator:    s.alloc,
		executor:     s.executor,
		ownership:    s.ownership,
		byBinding:    s.supervisor.byBinding,
		leaseHistory: cfg.Retention.Window(),
	}

	if err := s.reconciler.reconcile(ctx); err != nil {
		// Reconciliation starts a goroutine per adopted capsule before it
		// can fail, and those outlive this return: the drain below is not
		// registered yet, so nothing tells them the controller is going
		// away. Saying so here is what stops each of them writing an
		// error for every store and client call made under a process that
		// is already exiting -- a burst of failures describing the
		// shutdown rather than the start that failed.
		s.ownership.abandonUnfinished()
		return fmt.Errorf("startup reconciliation: %w", err)
	}

	// A session owner name, not an instance id. Concatenating an
	// untyped constant onto a typed string keeps the type, so this local
	// was an assignment.InstanceID that had to be converted back at the
	// call — a name derived from an id is not that id.
	owner := "runpool-" + string(st.InstanceID())[:8]
	s.supervisor.configureSessions(owner)
	// A session the broker still holds is one the next start has to wait
	// out, and it reports that wait as a predecessor's crash. Saying so
	// here is what tells the two apart.
	defer s.supervisor.closeSessions()

	var loops sync.WaitGroup
	// Opened after the startup reconcile, so a resolution can never land
	// while the books are still being brought back to what the host
	// shows. A controller that cannot open it serves without it: the
	// offline path still exists, and stopping every tenant's CI over a
	// listener that only saves a restart is the disproportion this
	// removes.
	if ln, err := listenForResolutions(opts.StateDir); err != nil {
		log.Warn("maintenance socket unavailable; resolving a held attempt needs the controller stopped",
			"error", err)
	} else {
		loops.Add(1)
		go func() {
			defer loops.Done()
			s.serveResolutions(ctx, ln)
		}()
	}
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.reconciler.run(ctx)
	}()
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.disk.run(ctx)
	}()
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.netSandbox.Watch(ctx, s.recordSandboxPass)
	}()
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.supervisor.run(ctx)
	}()
	loops.Wait()
	return s.drain()
}

func newAllocator(cfg *config.Config) *allocator.Allocator {
	if cfg.Scheduling.Parallelism == nil {
		return allocator.New()
	}
	return allocator.NewWithGlobalParallelism(*cfg.Scheduling.Parallelism)
}

// Controller coordinates process lifecycle across the scheduler, lease
// executor, reconciler, binding supervisor, and host monitors. Domain work
// stays in those components; this type owns their startup and shutdown order.
type Controller struct {
	log   *slog.Logger
	store *store.Store
	alloc *allocator.Allocator
	// executor owns one lease from its durable claim through disposition.
	executor *leaseExecutor
	// scheduler owns admission from the durable ready queue through the
	// committed handoff to capsule execution.
	scheduler *attemptScheduler
	// reconciler owns startup recovery and periodic convergence between the
	// durable store and runtime resources.
	reconciler *reconciler
	// supervisor owns configured bindings and their provider sessions.
	supervisor *bindingSupervisor

	// ownership coordinates active lease goroutines and recovery claims. The
	// controller lifecycle only waits for it or marks unfinished work for a
	// successor; launch and reconciliation own the individual claims.
	ownership *leaseOwnership

	// disk owns the pressure level and everything that moves it. The
	// controller reads the level in force on the admission path and does
	// not otherwise take part.
	disk *diskMonitor

	// netSandbox owns the restricted profile's egress policy: the deny
	// set, the snapshot each launch is cut from, and the installs into
	// running gateways. Nil is the explicit unsafe-open-egress profile,
	// which its own methods answer for.
	netSandbox *netsandbox.Manager

	// drainWindow overrides DrainTimeout in tests; zero means the
	// default. Held for the same reason as pollBackoff: a minute is not
	// something a test waits.
	drainWindow time.Duration
}

func (s *Controller) drain() error {
	s.log.Info("draining")
	window := s.drainWindow
	if window == 0 {
		window = DrainTimeout
	}
	select {
	case <-s.ownership.wait():
		s.log.Info("drained cleanly")
		return nil
	case <-time.After(window):
		// The goroutines that did not finish are woken moments from now
		// by the client and store closing under them, and their failure
		// paths would rewrite running leases on the way down. Marking
		// the abandonment first is what turns those paths into no-ops.
		s.ownership.abandonUnfinished()
		s.log.Warn("drain window elapsed; live capsules are left for the next start to adopt")
		return nil
	}
}
