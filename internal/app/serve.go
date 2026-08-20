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
	"sync/atomic"
	"time"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/doctor"
	"github.com/rhobuild/runpool/internal/lease"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
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
	// serving nothing for a reason no report would otherwise carry.
	sessionConflictGrace = 5 * time.Minute
)

type Options struct {
	Version      string
	CapsuleImage string
	StateDir     string
	Environ      func(string) string
}

// binding is one (target, tier) pair: its own scale set and session.
// The admission credit budget and advertised capacity are shared through
// the allocator, keyed by binding. Deduplication needs no memory here:
// delivery and attempt identity live in the store, and claiming an
// attempt is a compare-and-swap only one caller can win.
type binding struct {
	key    assignment.BindingKey
	target config.Target
	tier   config.Tier
	ref    config.TargetRef
	// gh is the provider client, held as the one interface above: the
	// binding must not know more about its provider than it serves
	// through, and a concrete client here was how partial test doubles
	// and untested paths accumulated.
	gh provider
	// scaleSetID is the provider's own id for this binding's scale set,
	// held in memory for the session and JIT calls. bindingID is what the
	// attempt and lease machinery keys on; this reaches the store in one
	// place only, inside the opaque delivery key, because a delivery id
	// is unique per queue rather than per binding and the binding
	// outlives the queue.
	scaleSetID int
	// scaleSetName is the provider-side name the loop creates or adopts
	// under. It is configuration rather than provider state, so it is
	// known before any call.
	scaleSetName string
	// ensured records that this process has created-or-adopted the scale
	// set. It starts false on every start, including a restart that read
	// scaleSetID from the store: the recorded id is proof of ownership,
	// not proof that the set still exists or still refuses runner
	// self-update. Owned by the binding's own loop.
	ensured bool
	// lastContactWrite paces the recorded successes. Owned by the
	// binding's own loop, like ensured above.
	lastContactWrite time.Time
	// reaching is what the store was last told about this binding's reach,
	// and lastFailure the reason it was last told about. Together they
	// make a write a transition rather than a repetition. Owned by the
	// binding's own loop.
	reaching    bool
	lastFailure string
	// conflictSince is when this binding's current run of broker session
	// conflicts began, zero while it holds none. A conflict is the
	// ordinary shape of a restart; one that outlasts the inactivity the
	// broker expires a session on is not, and only elapsed time tells the
	// two apart. Owned by the binding's own loop.
	conflictSince time.Time
	bindingID     assignment.BindingID
	cacheEnabled  bool
	generation    string
	// capsuleImage is what this binding launches: its tier's image where
	// one is configured, and the image this build ships otherwise. It is
	// resolved once, per binding, so the launch path never re-decides it.
	capsuleImage string
	// maxLanes is the most leases this binding's tier can run at once, which
	// is what bounds the cache lanes one project may hold. It is the tier's
	// parallelism capped by the instance-wide limit, because a lane per
	// possible tier lease overcounts when a global limit is the tighter one.
	maxLanes int

	session providerSession
	// newSession builds a session for this binding, in one attempt. The
	// loop's own backoff is the retry: a broker still holding this
	// binding's session is holding the one this process just closed, not
	// a predecessor's, and waiting that out in place would stall the loop
	// for as long as the broker takes. Held as a closure so the loop can
	// recover without knowing what a session is made of, and so a test
	// can hand it one that fails.
	newSession func(ctx context.Context) (providerSession, error)

	// lastAdvertised is what this binding announced last, so the credit
	// accounting is logged when it changes rather than every poll.
	// Owned by the binding's own loop.
	lastAdvertised int

	// mu serialises scheduling passes per binding, so one poll's serve
	// loop finishes deciding before the next begins.
	mu sync.Mutex
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
	var netSandbox *networkSandbox
	if cfg.Network.Profile == config.NetworkProfilePublicInternetOnly {
		netSandbox, err = newNetworkSandbox(ctx, dock, st.InstanceID(), capsuleImg, cfg, log)
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
		log:                 log,
		shippedCapsuleImage: capsuleImg,
		store:               st,
		objects:             dock,
		caps:                capsule.NewLauncher(dock, capsuleImg),
		wait:                dock,
		leases:              lease.NewManager(st, dock, log),
		cache:               cacheMgr,
		alloc:               newAllocator(cfg),
		leaseHistory:        cfg.Retention.Window(),
		netSandbox:          netSandbox,
		cgroupDriver:        hostInfo.CgroupDriver,
		byBinding:           map[assignment.BindingID]*binding{},
	}
	s.disk = newDiskMonitor(cfg, log, st, dock, cacheMgr, s.alloc, capsuleImg)
	// Before any binding serves: resuming an emergency is what holds the
	// credit pool shut, and admitting into one is the failure it prevents.
	if err := s.disk.resume(ctx); err != nil {
		return fmt.Errorf("resume disk pressure: %w", err)
	}
	s.launch = func(b *binding, lease store.Lease) { s.runCapsule(b, lease) }
	if err := s.buildBindings(ctx, cfg, opts.Environ); err != nil {
		return err
	}

	if err := s.reconcile(ctx); err != nil {
		return fmt.Errorf("startup reconciliation: %w", err)
	}

	// A session owner name, not an instance id. Concatenating an
	// untyped constant onto a typed string keeps the type, so this local
	// was an assignment.InstanceID that had to be converted back at the
	// call — a name derived from an id is not that id.
	owner := "runpool-" + string(st.InstanceID())[:8]
	for _, b := range s.bindings {
		b.newSession = func(ctx context.Context) (providerSession, error) {
			return b.gh.OpenSession(ctx, b.scaleSetID, owner)
		}
	}
	// A session the broker still holds is one the next start has to wait
	// out, and it reports that wait as a predecessor's crash. Saying so
	// here is what tells the two apart.
	defer s.closeSessions()

	var loops sync.WaitGroup
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.periodicReconcile(ctx)
	}()
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.disk.run(ctx)
	}()
	loops.Add(1)
	go func() {
		defer loops.Done()
		s.netSandbox.watch(ctx)
	}()
	for _, b := range s.bindings {
		loops.Add(1)
		go func(b *binding) {
			defer loops.Done()
			s.loop(ctx, b)
		}(b)
	}
	loops.Wait()
	return s.drain()
}

func newAllocator(cfg *config.Config) *allocator.Allocator {
	if cfg.Scheduling.Parallelism == nil {
		return allocator.New()
	}
	return allocator.NewWithGlobalParallelism(*cfg.Scheduling.Parallelism)
}

// capsuleRuntime is what serving needs from the capsule layer, defined
// here because the consumer owns its interfaces. The fault matrix
// depends on this seam: preparation, start and inspection failures are
// exactly the outcomes Docker cannot be asked to produce on demand.
type capsuleRuntime interface {
	Prepare(ctx context.Context, spec capsule.Spec, rec capsule.ResourceRecorder) (capsule.PreparedRuntime, error)
	Start(ctx context.Context, prepared capsule.PreparedRuntime) error
	InspectExecution(ctx context.Context, prepared capsule.PreparedRuntime) (assignment.ExecutionObservation, error)
}

// runtimeWaiter is the slice of the Docker client the serving loop
// awaits runners through.
type runtimeWaiter interface {
	WaitExit(ctx context.Context, id string) (int64, error)
	TailLogs(ctx context.Context, id string, lines int) (string, error)
}

// provider is everything a binding needs from its provider client, as
// one interface: the scale set it creates or adopts, the JIT
// credentials it mints, the runners it deregisters, and the message
// session it polls. One seam rather than a concrete client beside two
// partial views, because they are one client holding one credential —
// and because what the loop has to get right is the provider failing,
// which is only testable against a provider that fails on demand. The
// session keeps its own seam through newSession: the loop's session
// handling is a subject of its own.
type provider interface {
	EnsureScaleSet(ctx context.Context, groupName, name string, knownID int, intended bool) (githubactions.ScaleSet, error)
	GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName, workFolder string) (githubactions.JITConfig, error)
	RemoveRunner(ctx context.Context, id int) error
	OpenSession(ctx context.Context, scaleSetID int, owner string) (*githubactions.Session, error)
}

// providerSession is the message session as the serving loop uses it.
//
// Declared here, like the seams above, because what the loop has to get
// right is the session failing: the upstream client refreshes an expired
// session by itself, and when that refresh fails it leaves the handle
// dead. Polling a dead handle is indistinguishable from a quiet provider
// unless the loop can be shown one.
type providerSession interface {
	Receive(ctx context.Context) (*githubactions.Message, error)
	Acknowledge(ctx context.Context, messageID int) error
	SetCapacity(n int)
	Initial() *githubactions.Statistics
	Close(ctx context.Context) error
}

// ownedObjects is what reconciliation needs from the daemon: the
// inventory of everything this instance labelled, and the removal of one
// of them. Nothing here creates.
//
// Narrow, and declared by the consumer, for the same reason as the
// seams above: what reconciliation has to get right is the daemon
// refusing — a container that will not die, an inventory that cannot be
// read — and a live daemon cannot be asked for those on demand. Held as
// this interface rather than the client itself, the whole startup and
// sweep path becomes reachable from a test.
type ownedObjects interface {
	ListOwnedContainers(ctx context.Context, instanceID assignment.InstanceID) ([]docker.OwnedContainer, error)
	ListOwnedNetworks(ctx context.Context, instanceID assignment.InstanceID) ([]docker.OwnedResource, error)
	ListOwnedVolumes(ctx context.Context, instanceID assignment.InstanceID) ([]docker.OwnedResource, error)
	RemoveContainer(ctx context.Context, id string) error
	RemoveNetwork(ctx context.Context, id string) error
	RemoveVolume(ctx context.Context, name string) error
}

// Controller is the running instance: it owns the durable store, the
// daemon connection, the credit pool and the loops that keep them
// agreeing with each other. One process, one state directory, one
// Controller — the singleton lock enforces it.
type Controller struct {
	log *slog.Logger
	// shippedCapsuleImage is the capsule this build carries. A binding
	// launches its own resolved image; this is what a tier falls back to,
	// and what the disk monitor probes with.
	shippedCapsuleImage string
	// pollBackoff is the pause between failed polls; zero means the
	// package default. It exists so the session-recovery path is
	// reachable from a test without waiting out real seconds.
	pollBackoff time.Duration
	// conflictBackoff overrides sessionConflictBackoff for the loop's
	// reopen path; zero means the package default. Held for the same
	// reason as pollBackoff: ten seconds is not something a test waits.
	conflictBackoff time.Duration
	// conflictGrace overrides sessionConflictGrace; zero means the
	// package default. Held for the same reason as the two above: five
	// minutes is not something a test waits, and what happens past it is
	// the whole of this behaviour.
	conflictGrace time.Duration
	store         *store.Store
	// objects is the daemon as reconciliation sees it: an inventory and
	// a way to remove from it. Creating capsules goes through caps, and
	// awaiting them through wait.
	objects ownedObjects
	caps    capsuleRuntime
	wait    runtimeWaiter
	// leases owns the lease machine and the cleanup saga. Everything
	// about ending a capsule's life lives there; the controller decides
	// when, and who has to be told.
	leases *lease.Manager
	cache  *cache.LaneManager
	alloc  *allocator.Allocator

	// inFlight holds the id of every lease a goroutine is currently
	// driving. It is what tells the periodic reconciler which live leases
	// are stranded — nobody's — and which are simply being worked on, and
	// it is the mutual exclusion that keeps two owners from releasing the
	// same admission credit twice.
	//
	// A typed map under a mutex rather than a sync.Map: a sync.Map keys on
	// `any`, so a caller passing a plain string where the id type is
	// meant compiles, misses every entry, and hands two owners the same
	// lease. The type checker covers the key here, and the map is only
	// ever as large as the live leases, so the lock is uncontended.
	inFlightMu sync.Mutex
	inFlight   map[assignment.LeaseID]struct{}

	// disk owns the pressure level and everything that moves it. The
	// controller reads the level in force on the admission path and does
	// not otherwise take part.
	disk *diskMonitor

	// leaseHistory is how long a finished lease's record is kept; zero
	// keeps every one. Captured at construction rather than read back out
	// of the configuration, because the only *config.Config the
	// controller holds belongs to the network sandbox.
	leaseHistory time.Duration

	// netSandbox owns the restricted profile's egress policy: the deny
	// set, the snapshot each launch is cut from, and the installs into
	// running gateways. Nil is the explicit unsafe-open-egress profile,
	// which its own methods answer for.
	netSandbox *networkSandbox

	// cgroupDriver is the daemon's driver, read once at startup. It
	// decides the form of a lease's parent cgroup, and the daemon
	// rejects the wrong form outright.
	cgroupDriver string

	bindings  []*binding
	byBinding map[assignment.BindingID]*binding

	// launch runs one served attempt. Production points it at
	// runCapsule; tests replace it to drive the assignment machine
	// through failures Docker and GitHub cannot be asked to produce.
	launch func(b *binding, lease store.Lease)

	wg sync.WaitGroup // capsule goroutines, for drain

	// abandoning is set when the drain window elapses. From then on the
	// destructive recovery paths leave leases exactly as they are: the
	// successor's adoption owns recovery now, and a dying process that
	// rewrites a running lease to cleaning hands the next start a
	// failure to dismantle instead of a capsule to adopt.
	abandoning atomic.Bool

	// drainWindow overrides DrainTimeout in tests; zero means the
	// default. Held for the same reason as pollBackoff: a minute is not
	// something a test waits.
	drainWindow time.Duration

	// strandedGrace overrides defaultStrandedGrace in tests; zero means
	// the default. A test that fabricates a stranded lease writes it in
	// the same instant it sweeps for one, and the production window is
	// two minutes.
	strandedGrace time.Duration
}

// strandedAfter is how long a live lease must have gone untouched
// before the periodic pass may claim it as ownerless.
func (s *Controller) strandedAfter() time.Duration {
	if s.strandedGrace > 0 {
		return s.strandedGrace
	}
	return defaultStrandedGrace
}

func (s *Controller) drain() error {
	s.log.Info("draining")
	window := s.drainWindow
	if window == 0 {
		window = DrainTimeout
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		s.log.Info("drained cleanly")
		return nil
	case <-time.After(window):
		// The goroutines that did not finish are woken moments from now
		// by the client and store closing under them, and their failure
		// paths would rewrite running leases on the way down. Marking
		// the abandonment first is what turns those paths into no-ops.
		s.abandoning.Store(true)
		s.log.Warn("drain window elapsed; live capsules are left for the next start to adopt")
		return nil
	}
}

// closeSessions closes every binding's message session concurrently
// under one shared budget. The loops have already been waited out, so
// each binding's session field is quiescent here.
func (s *Controller) closeSessions() {
	ctx, cancel := context.WithTimeout(context.Background(), SessionCloseBudget)
	defer cancel()
	var wg sync.WaitGroup
	for _, b := range s.bindings {
		if b.session == nil {
			continue // never opened, or discarded by the loop
		}
		wg.Add(1)
		go func(b *binding) {
			defer wg.Done()
			if err := b.session.Close(ctx); err != nil {
				s.log.Warn("cannot close the message session; the broker holds it until it "+
					"expires and the next start waits that out",
					"binding", b.key, "error", err)
			}
		}(b)
	}
	wg.Wait()
}

// claimLease takes ownership of a lease's recovery. It reports false when
// another goroutine already holds it, which is what keeps the periodic
// reconciler off a lease that is still being driven — and keeps two owners
// from each concluding the lease is done and releasing its credit.
func (s *Controller) claimLease(leaseID assignment.LeaseID) bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	if _, held := s.inFlight[leaseID]; held {
		return false
	}
	if s.inFlight == nil {
		s.inFlight = map[assignment.LeaseID]struct{}{}
	}
	s.inFlight[leaseID] = struct{}{}
	return true
}

func (s *Controller) releaseLease(leaseID assignment.LeaseID) {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	delete(s.inFlight, leaseID)
}
