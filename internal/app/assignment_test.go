package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/githubactions"
	"github.com/rhobuild/runpool/internal/lease"
	"github.com/rhobuild/runpool/internal/store"
)

// These tests exercise the assignment machine across the app layer, not
// only its SQLite primitives: what is acknowledged, what becomes a lease,
// what survives a restart, and what happens when each step fails — a
// message can be acknowledged in one layer while the work is lost in
// another, and only a cross-layer test sees that.
//
// GitHub and Docker are absent by construction: deliveries enter through
// the same persistence path production uses, and the loop's decisions
// are driven through the launch seam and the durable store, which are
// where the properties live.

type harness struct {
	t     *testing.T
	srv   *Controller
	bind  *binding
	store *store.Store
	// probe is the filesystem the disk monitor sees. A test states the
	// facts it wants and the monitor decides from them.
	probe *fakeProbe
	// objects is the daemon inventory reconciliation works from.
	objects *fakeObjects

	mu       sync.Mutex
	launched []assignment.AttemptID
	leases   map[assignment.AttemptID]store.Lease // attempt id -> lease, captured at launch

	msgSeq int
}

func newHarness(t *testing.T, parallelism int) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newHarnessOnStore(t, st, parallelism)
}

// newHarnessOnStore builds a controller over an existing database — the
// restart path: the same binding identity resolves to the same rows.
func newHarnessOnStore(t *testing.T, st *store.Store, parallelism int) *harness {
	t.Helper()
	var bindingID assignment.BindingID
	if err := st.Tx(context.Background(), func(tx *store.Tx) error {
		var err error
		bindingID, err = tx.EnsureBinding("app", "github_actions",
			"v1|repository|https://github.com/acme/app||runpool-standard")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, store: st, leases: map[assignment.AttemptID]store.Lease{}}
	b := &binding{
		key:       "app/standard",
		tier:      config.Tier{ID: "standard", Parallelism: parallelism},
		bindingID: bindingID,
		// These tests serve an existing binding; creating or adopting its
		// scale set is the subject of its own test.
		ensured: true,
	}
	h.bind = b
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cacheMgr := cache.New(st, nopVolumes{}, st.InstanceID())
	h.objects = &fakeObjects{wedged: map[string]bool{}}
	byBinding := map[assignment.BindingID]*binding{bindingID: b}
	h.srv = &Controller{
		log:       log,
		store:     st,
		alloc:     allocator.New(),
		ownership: newLeaseOwnership(),
	}
	h.srv.executor = &leaseExecutor{
		log:       log,
		store:     st,
		leases:    lease.NewManager(st, nopRemover{}, log),
		cache:     cacheMgr,
		allocator: h.srv.alloc,
		ownership: h.srv.ownership,
	}
	h.srv.reconciler = &reconciler{
		log:       log,
		store:     st,
		objects:   h.objects,
		allocator: h.srv.alloc,
		executor:  h.srv.executor,
		ownership: h.srv.ownership,
		byBinding: byBinding,
		// A lease this harness calls stranded was written moments ago,
		// because no test here simulates the passage of time. The grace
		// exists for a real gap between a lease committing and its owner
		// registering, and a test whose subject is that gap sets its own.
		strandedGrace: time.Nanosecond,
	}
	if err := h.srv.alloc.Register(assignment.TierID(b.tier.ID), b.key, parallelism); err != nil {
		t.Fatal(err)
	}
	h.srv.alloc.SessionOpened(b.key)
	// The monitor is real, on a defaulted policy and a probe that reports
	// room: admission consults the level on every delivery, and a nil
	// monitor would make "no pressure" indistinguishable from "not wired".
	monitorCfg := &config.Config{}
	config.ApplyDefaults(monitorCfg)
	h.probe = &fakeProbe{free: engine.FilesystemFree{FreeBytes: 1 << 40, FreeInodes: 1 << 20}}
	h.srv.disk = newDiskMonitor(monitorCfg, log, st, h.probe, cacheMgr, h.srv.alloc, "probe-image")
	if err := h.srv.disk.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The default launch records the claim and leaves the lease running,
	// so a test decides its outcome explicitly.
	h.srv.scheduler = &attemptScheduler{
		log:         log,
		store:       st,
		allocator:   h.srv.alloc,
		ownership:   h.srv.ownership,
		pressure:    h.srv.currentPressure,
		createLease: h.srv.executor.createLease,
		launch: func(_ *binding, lease store.Lease) {
			h.mu.Lock()
			h.launched = append(h.launched, lease.AttemptID)
			h.leases[lease.AttemptID] = lease
			h.mu.Unlock()
		},
	}
	h.srv.supervisor = &bindingSupervisor{
		log:       log,
		store:     st,
		allocator: h.srv.alloc,
		scheduler: h.srv.scheduler,
		bindings:  []*binding{b},
		byBinding: byBinding,
	}
	return h
}

// deliverMsg persists one broker message through the production path.
// The message id is explicit so a test can redeliver the same message.
func (h *harness) deliverMsg(msgID assignment.SourceDeliveryID,
	workloads ...assignment.WorkloadAssignment) error {
	_, err := h.srv.supervisor.persistDelivery(h.t.Context(), h.bind, &githubactions.Message{
		ID: msgID, Assigned: workloads,
	})
	return err
}

// deliver persists one fresh message.
func (h *harness) deliver(workloads ...assignment.WorkloadAssignment) error {
	h.msgSeq++
	return h.deliverMsg(assignment.SourceDeliveryID(h.msgSeq), workloads...)
}

func (h *harness) serve() {
	h.srv.scheduler.schedule(h.t.Context(), h.bind)
	<-h.srv.ownership.wait()
}

func (h *harness) launchedAttempts() []assignment.AttemptID {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]assignment.AttemptID(nil), h.launched...)
}

// useRemover rebuilds the lease manager over a faulted daemon, so a test
// can drive the cleanup saga through removal failures.
func (h *harness) useRemover(r lease.Remover) {
	h.srv.executor.leases = lease.NewManager(h.store, r, h.srv.log)
}

func (h *harness) recordEvidence(leaseID assignment.LeaseID, e store.Evidence) {
	h.t.Helper()
	if err := h.store.Tx(h.t.Context(), func(tx *store.Tx) error {
		return tx.RecordEvidenceForLease(leaseID, e)
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) inStore(fn func(*store.Tx) error) {
	h.t.Helper()
	if err := h.store.Tx(context.Background(), fn); err != nil {
		h.t.Fatal(err)
	}
}

// ready lists the binding's servable attempts.
func (h *harness) ready() []store.Attempt {
	h.t.Helper()
	var out []store.Attempt
	h.inStore(func(s *store.Tx) error {
		var err error
		out, err = s.AllReadyAttempts(h.bind.bindingID)
		return err
	})
	return out
}

// attemptByLease resolves the attempt a lease serves, or a zero Attempt
// when disposition already detached it.
func (h *harness) attemptByLease(leaseID assignment.LeaseID) store.Attempt {
	h.t.Helper()
	var out store.Attempt
	if err := h.store.Tx(context.Background(), func(tx *store.Tx) error {
		a, err := tx.AttemptByLease(leaseID)
		switch {
		case err == nil:
			out = a
			return nil
		case errors.Is(err, store.ErrNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// resolveWithoutRuntime runs recovery for a lease whose capsule
// no longer exists — the common crash shape these tests construct.
func (h *harness) resolveWithoutRuntime(ctx context.Context, lease store.Lease) {
	h.srv.reconciler.resolveInterrupted(ctx, h.bind, lease, engine.OwnedContainer{}, false)
}

func demand(workloadKey, project string, run int64) assignment.WorkloadAssignment {
	return assignment.WorkloadAssignment{
		SourceWorkloadKey: assignment.SourceWorkloadKey(workloadKey), TenantKey: "acme",
		ProjectKey: assignment.ProjectKey(project), SourceRunID: run,
	}
}

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

var errDaemon = errors.New("daemon unreachable")

// fakeObjects stands in for the daemon's inventory. It records what was
// removed and can refuse any of it by name, which is the half of
// reconciliation that matters and the half a live daemon will not
// perform on request: an object that does not die.
type fakeObjects struct {
	containers []engine.OwnedContainer
	networks   []engine.OwnedResource
	volumes    []engine.OwnedResource
	// wedged names the objects whose removal fails, by id.
	wedged map[string]bool
	// listErr fails the container inventory, which is the failure a sweep
	// must treat as fatal rather than as an empty daemon.
	listErr error
	// onList fires as the daemon answers, once per kind, which is where
	// a lease that commits mid-sweep falls.
	onList func()

	removed []string
}

func (f *fakeObjects) ListOwnedContainers(context.Context, assignment.InstanceID) ([]engine.OwnedContainer, error) {
	if f.onList != nil {
		f.onList()
	}
	return f.containers, f.listErr
}
func (f *fakeObjects) ListOwnedNetworks(context.Context, assignment.InstanceID) ([]engine.OwnedResource, error) {
	if f.onList != nil {
		f.onList()
	}
	return f.networks, nil
}
func (f *fakeObjects) ListOwnedVolumes(context.Context, assignment.InstanceID) ([]engine.OwnedResource, error) {
	if f.onList != nil {
		f.onList()
	}
	return f.volumes, nil
}
func (f *fakeObjects) RemoveContainer(_ context.Context, id string) error { return f.remove(id) }
func (f *fakeObjects) RemoveNetwork(_ context.Context, id string) error   { return f.remove(id) }
func (f *fakeObjects) RemoveVolume(_ context.Context, name string) error  { return f.remove(name) }

func (f *fakeObjects) remove(id string) error {
	if f.wedged[id] {
		return errors.New("object is wedged")
	}
	f.removed = append(f.removed, id)
	return nil
}

// fakeProbe stands in for the daemon on the measurement side. Its whole
// purpose is the states a real filesystem cannot be asked for on demand:
// no bytes left, no inodes left, a probe that fails.
type fakeProbe struct {
	free     engine.FilesystemFree
	usage    []engine.VolumeUsage
	freeErr  error
	usageErr error
}

func (f *fakeProbe) ProbeFilesystemFree(context.Context, string, assignment.InstanceID) (engine.FilesystemFree, error) {
	return f.free, f.freeErr
}

func (f *fakeProbe) OwnedVolumeUsage(context.Context, assignment.InstanceID) ([]engine.VolumeUsage, error) {
	return f.usage, f.usageErr
}

// nopVolumes stands in for the daemon on the cache side: every lane
// volume exists and is owned. These tests exercise assignment, not
// cache handling.
type nopVolumes struct{}

func (nopVolumes) EnsureOwnedVolume(context.Context, string, map[string]string) error { return nil }
func (nopVolumes) OwnedIDByName(_ context.Context, _ engine.ObjectKind, name string, _ assignment.InstanceID, _ assignment.LeaseID) (string, error) {
	return name, nil
}
func (nopVolumes) RemoveVolume(context.Context, string) error { return nil }
func (nopVolumes) OwnedVolumeUsage(context.Context, assignment.InstanceID) ([]engine.VolumeUsage, error) {
	return nil, nil
}

// A message whose assignments cannot all be made durable must not be
// acknowledged. Dropping the unrecordable one and acknowledging the rest
// loses work silently, which is what the code did before.
func TestUnidentifiableAssignmentFailsTheMessage(t *testing.T) {
	h := newHarness(t, 2)

	err := h.deliver(
		demand("job-1", "app", 10),
		demand("", "app", 11), // no identity
	)
	if err == nil {
		t.Fatal("persisting succeeded despite an assignment with no identity; the message would be acknowledged")
	}
	if got := h.ready(); len(got) != 0 {
		t.Errorf("a partial record was committed: %+v", got)
	}
}

// Redelivery after a failed acknowledgement must not duplicate work, and
// two jobs of one workflow run must both be served.
func TestRedeliveryIsIdempotentAndMatrixJobsBothRun(t *testing.T) {
	h := newHarness(t, 2)
	linux, macos := demand("job-linux", "app", 99), demand("job-macos", "app", 99)

	if err := h.deliverMsg(7, linux, macos); err != nil {
		t.Fatal(err)
	}
	// The acknowledgement failed, so the broker sends the same message
	// again, byte for byte.
	if err := h.deliverMsg(7, linux, macos); err != nil {
		t.Fatal(err)
	}
	h.serve()

	if got := h.launchedAttempts(); len(got) != 2 {
		t.Fatalf("launched %v; want both matrix jobs exactly once", got)
	}
	// Serving again must not launch anything: the work is in flight.
	h.serve()
	if got := h.launchedAttempts(); len(got) != 2 {
		t.Errorf("serving twice launched %v", got)
	}
}

// The admission credit budget gates launches; the attempt stays durable
// and is served when a credit frees.
func TestAttemptsWaitForACreditAndSurvive(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-a", "app", 1)); err != nil {
		t.Fatal(err)
	}
	if err := h.deliver(demand("job-b", "app", 2)); err != nil {
		t.Fatal(err)
	}
	h.serve()

	if got := h.launchedAttempts(); len(got) != 1 {
		t.Fatalf("launched %v with one credit; want one", got)
	}
	if got := h.ready(); len(got) != 1 {
		t.Errorf("the waiting attempt is not queued: %+v", got)
	}
}

func TestSchedulerDrainsSeveralBoundedBatches(t *testing.T) {
	const attempts = maxReadyAttemptBatch*2 + 2
	h := newHarness(t, attempts)
	workloads := make([]assignment.WorkloadAssignment, attempts)
	for index := range workloads {
		workloads[index] = demand(fmt.Sprintf("job-%03d", index), "app", int64(index+1))
	}
	if err := h.deliver(workloads...); err != nil {
		t.Fatal(err)
	}
	h.serve()
	if got := len(h.launchedAttempts()); got != attempts {
		t.Fatalf("launched %d attempts; want all %d across bounded batches", got, attempts)
	}
	if ready := h.ready(); len(ready) != 0 {
		t.Fatalf("%d attempts remain ready after capacity admitted the complete backlog", len(ready))
	}
}

// The lease machine's own contracts — disposition by evidence, the
// atomicity of the finalizing transaction, the cleanup saga's backoff —
// live with the code that owns them, in internal/lease. What stays here
// is what only the composition layer can exercise: the serving loop, the
// provider-facing paths, and startup recovery across bindings.

// A controller that dies with attempts recorded but unserved must resume
// them, which is the whole point of persisting before the
// acknowledgement. The restart is real: the database closes and reopens
// from disk, and a fresh coordinator resolves the same binding identity
// to the same rows.
func TestReadyAttemptsResumeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOnStore(t, st, 1)
	if err := h.deliver(demand("job-crash", "app", 7)); err != nil {
		t.Fatal(err)
	}
	// The process dies here: nothing was launched, and GitHub considers
	// the message delivered.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	revived := newHarnessOnStore(t, reopened, 1)
	revived.serve()
	got := revived.launchedAttempts()
	if len(got) != 1 {
		t.Fatalf("after restart launched %v; want the recorded attempt", got)
	}
	if a := revived.attemptByLease(revived.leases[got[0]].ID); a.SourceWorkloadKey != "job-crash" {
		t.Fatalf("launched workload = %q; want job-crash", a.SourceWorkloadKey)
	}
}

// attemptState reloads one attempt by id, wherever its state moved.
func attemptState(t *testing.T, h *harness, attemptID assignment.AttemptID) store.Attempt {
	t.Helper()
	var out store.Attempt
	h.inStore(func(s *store.Tx) error {
		a, err := s.Get(attemptID)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	return out
}

// leaseFor claims the open attempt of a workload through the production
// transaction, returning the lease and the attempt id.
func leaseFor(t *testing.T, h *harness, workloadKey assignment.SourceWorkloadKey) (store.Lease, assignment.AttemptID) {
	t.Helper()
	var lease store.Lease
	var attemptID assignment.AttemptID
	if err := h.store.Tx(context.Background(), func(tx *store.Tx) error {
		ready, err := tx.AllReadyAttempts(h.bind.bindingID)
		if err != nil {
			return err
		}
		for _, a := range ready {
			if a.SourceWorkloadKey != workloadKey {
				continue
			}
			attemptID = a.ID
			lease, err = tx.LeaseAttempt(a.ID, h.bind.bindingID, h.bind.tier.ID)
			return err
		}
		t.Fatalf("no ready attempt for workload %q", workloadKey)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return lease, attemptID
}

func driveLeaseTo(t *testing.T, h *harness, leaseID assignment.LeaseID, target store.LeaseState) {
	t.Helper()
	// A lease starts reserved; every other state is reached by walking the
	// real machine, so a test never fabricates a state the product cannot
	// produce. Hand-written per-target paths silently left several states
	// at reserved, which made a reconciliation test assert nothing.
	paths := map[store.LeaseState][]store.LeaseState{
		store.LeaseReserved:          {},
		store.LeaseProvisioning:      {store.LeaseProvisioning},
		store.LeaseRuntimeRegistered: {store.LeaseProvisioning, store.LeaseRuntimeRegistered},
		store.LeaseWorkloadRunning:   {store.LeaseProvisioning, store.LeaseRuntimeRegistered, store.LeaseWorkloadRunning},
		store.LeaseDraining: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining},
		store.LeaseCleaning: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining, store.LeaseCleaning},
		store.LeaseReleased: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining, store.LeaseCleaning, store.LeaseReleased},
		store.LeaseFailed: {store.LeaseProvisioning, store.LeaseFailed},
		store.LeaseQuarantined: {store.LeaseProvisioning, store.LeaseRuntimeRegistered,
			store.LeaseWorkloadRunning, store.LeaseDraining, store.LeaseCleaning, store.LeaseQuarantined},
	}
	path, ok := paths[target]
	if !ok {
		t.Fatalf("no path defined to lease state %q", target)
	}
	if err := h.store.Tx(context.Background(), func(tx *store.Tx) error {
		from := store.LeaseReserved
		for _, to := range path {
			if err := tx.TransitionLease(leaseID, from, to); err != nil {
				return err
			}
			from = to
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := reloadLease(t, h, leaseID); got.State != target {
		t.Fatalf("wanted a lease in %q but it is %q; the test would assert nothing", target, got.State)
	}
}

// resolveWithRuntime is the other half of resolveWithoutRuntime: the
// crash left a container, so the pass can ask it whether the authorized
// start took effect. The observation the capsule answers with is the
// caller's to choose, because it is the fact under test.
func (h *harness) resolveWithRuntime(ctx context.Context, lease store.Lease, obs assignment.ExecutionObservation) {
	h.srv.executor.capsule = &fakeCapsule{obs: obs}
	h.srv.reconciler.resolveInterrupted(ctx, h.bind, lease,
		engine.OwnedContainer{ID: "runner-x", Role: engine.RoleCapsule, Running: false}, true)
}
