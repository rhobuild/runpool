package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

// Regressions for the ways an assignment can be lost — as opposed to
// run twice, the failure the design guarded first. None of them is
// visible from the outside: the controller reports a clean release
// while the work never happens. They shared one root: identity keyed on
// the job id alone, and recovery inferred from the lease state a crash
// happened to leave behind rather than from what execution was
// observed.

// A lease that reached failed without ever provisioning a runner
// carried no execution. Cleanup state is not execution evidence: this
// attempt was never consumed, so it belongs back in the queue.
func TestFailedBeforeAnyRunnerRequeuesItsAttempt(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-prerun", "app", 40)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-prerun")
	// reserved -> provisioning -> failed: no runner was ever registered,
	// so nothing can have executed.
	driveLeaseTo(t, h, lease.ID, store.LeaseFailed)

	h.resolveWithoutRuntime(t.Context(), reloadLease(t, h, lease.ID))

	ready := h.ready()
	if len(ready) != 1 {
		t.Fatalf("ready attempts = %d; want 1 — a lease that failed before "+
			"provisioning a runner never consumed its delivery, and settling it "+
			"drops the job with no trace", len(ready))
	}

	// Servable means the scheduler leases it again. Asserting only that it
	// is back in the queue left the second serving untested, and it could
	// not happen: the attempt already held a lease, and one per attempt
	// was all the schema allowed.
	h.srv.scheduleReadyAttempts(t.Context(), h.bind)
	if got := len(h.ready()); got != 0 {
		t.Fatalf("%d attempts still ready after a scheduling pass; the requeued "+
			"attempt was not served again, so it waits at the head of a queue "+
			"nothing drains", got)
	}
	var second store.Lease
	h.inStore(func(tx *store.Tx) error {
		var err error
		second, err = tx.LeaseByAttempt(ready[0].ID)
		return err
	})
	if second.ID == lease.ID {
		t.Error("the second serving reused the first lease; each serving is its own")
	}
}

// Reconciliation runs at startup, where a shutdown can cancel the
// context mid-pass. Releasing the lease and requeueing its attempt are
// one recovery: if the release survives cancellation but the requeue
// does not, the attempt stays leased to a lease that no longer exists
// and no query will ever return it again.
func TestRequeueSurvivesACancelledReconciliation(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-cancelled", "app", 41)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-cancelled")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.resolveWithoutRuntime(ctx, reloadLease(t, h, lease.ID))

	if got := reloadLease(t, h, lease.ID); got.State != store.LeaseReleased {
		t.Fatalf("lease ended %s; the release itself is expected to survive cancellation", got.State)
	}
	if got := len(h.ready()); got != 1 {
		t.Errorf("ready attempts = %d; want 1 — the lease was released but its "+
			"attempt stayed leased to it, so the job is now invisible to "+
			"every query and is never retried", got)
	}
}

// GitHub can cancel an assignment and reassign the same workflow job up
// to three times. Each reassignment arrives as a new delivery. The
// settled predecessor must not hide the new attempt, and the new
// attempt must carry its own observed identifiers.
func TestReassignmentOfASettledJobIsServable(t *testing.T) {
	h := newHarness(t, 1)

	first := demand("job-reassigned", "app", 42)
	first.SourceRequestID = 201
	if err := h.deliverMsg(1, first); err != nil {
		t.Fatal(err)
	}
	// The first attempt runs and settles: a redelivery of *that*
	// delivery must never start a second capsule, and does not here.
	lease, attemptID := leaseFor(t, h, "job-reassigned")
	driveLeaseTo(t, h, lease.ID, store.LeaseCleaning)
	h.recordEvidence(lease.ID, store.EvidenceExitObserved)
	if err := h.srv.leases.Finalize(t.Context(), lease.ID, assignment.NoObservation); err != nil {
		t.Fatal(err)
	}
	if got := len(h.ready()); got != 0 {
		t.Fatalf("a settled attempt is still servable (%d); the tombstone is not holding", got)
	}

	// GitHub cancels and reassigns the same job: same workload key, new
	// message, new request id. This is work that has never run.
	second := demand("job-reassigned", "app", 42)
	second.SourceRequestID = 202
	if err := h.deliverMsg(2, second); err != nil {
		t.Fatal(err)
	}

	ready := h.ready()
	if len(ready) != 1 {
		t.Fatalf("ready attempts = %d; want 1 — a reassignment is a new attempt, "+
			"and deduplicating on the workload key alone hides it behind the "+
			"attempt that already completed", len(ready))
	}
	if ready[0].SourceWorkloadKey != "job-reassigned" || ready[0].ID == attemptID {
		t.Errorf("servable attempt = %+v; want a fresh attempt of the same workload", ready[0])
	}
}

// A reassignment can also race a predecessor that was assigned but
// never served. The predecessor consumed nothing, so it is superseded
// in the same transaction that records the new delivery — never do two
// attempts of one workload serve together.
func TestReassignmentSupersedesAReadyPredecessor(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.deliverMsg(1, demand("job-raced", "app", 43)); err != nil {
		t.Fatal(err)
	}
	old := h.ready()[0]

	if err := h.deliverMsg(2, demand("job-raced", "app", 43)); err != nil {
		t.Fatal(err)
	}

	ready := h.ready()
	if len(ready) != 1 || ready[0].ID == old.ID {
		t.Fatalf("ready = %+v; want exactly the new attempt", ready)
	}
	if got := attemptState(t, h, old.ID); got.State != store.AttemptSuperseded || got.Resolution != assignment.ResolutionSuperseded {
		t.Errorf("predecessor = %s/%s; want superseded/%s", got.State, got.Resolution, assignment.ResolutionSuperseded)
	}
}

// Each of the following is a distinct way for an attempt to be resolved
// as if its job had run, or to stop being visible at all, while the
// controller reports success.

// A capsule is a network, five volumes, a daemon, a container and a
// credential before anything is asked to start. Recording a start at the
// top of that stretch means every failure inside it — a volume, the
// daemon, the JIT delivery — reads as work that may have executed, and
// the job is settled instead of retried.
func TestPreparationFailureLeavesTheAttemptServable(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-prepare", "app", 50)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-prepare")
	driveLeaseTo(t, h, lease.ID, store.LeaseRuntimeRegistered)
	// The capsule was built; no start was ever authorized.
	h.recordEvidence(lease.ID, store.EvidenceRuntimePrepared)

	h.resolveWithoutRuntime(t.Context(), reloadLease(t, h, lease.ID))

	if got := len(h.ready()); got != 1 {
		t.Errorf("ready attempts = %d; want 1 — a capsule that was prepared but "+
			"never authorized to start consumed no delivery", got)
	}
}

// recoverCapsuleFailure can carry a lease from provisioning to failed and on to
// cleaning without a runner ever existing, so "this far along" says
// nothing about execution. Cleanup removes resources; it must not decide
// what became of the work.
func TestCleaningDoesNotDecideExecution(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-cleaning", "app", 51)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-cleaning")
	driveLeaseTo(t, h, lease.ID, store.LeaseCleaning)
	h.recordEvidence(lease.ID, store.EvidenceNotStarted)

	h.resolveWithoutRuntime(t.Context(), reloadLease(t, h, lease.ID))

	if got := len(h.ready()); got != 1 {
		t.Errorf("ready attempts = %d; want 1 — a lease reaches cleaning from "+
			"recoverCapsuleFailure too, and settling every lease this far along as executed "+
			"drops the jobs that never ran", got)
	}
}

// Releasing a lease and disposing of its attempt are two commits. A
// crash in between leaves the attempt leased to a lease that is
// terminal, and therefore outside every working set the next startup
// builds: no query returns it and nothing ever retries it.
func TestAttemptStrandedOnAReleasedLeaseIsRecovered(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-stranded", "app", 52)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-stranded")
	driveLeaseTo(t, h, lease.ID, store.LeaseReleased)
	h.recordEvidence(lease.ID, store.EvidenceNotStarted)

	// This is the reachable state: the lease is released, the attempt
	// still leased to it, and nothing disposed of it.
	var stranded int
	h.inStore(func(s *store.Tx) error {
		attempts, err := s.StrandedAttempts()
		if err != nil {
			return err
		}
		stranded = len(attempts)
		return nil
	})
	if stranded != 1 {
		t.Fatalf("the invariant sweep found %d stranded attempts; want 1 — an attempt "+
			"leased to a released lease is invisible to every other query", stranded)
	}

	// The real startup pass, not the resolver handed the lease: finding
	// the stranded attempt is the half that matters. Its lease is
	// released, so it is outside the live working set, invisible to
	// ReadyAttempts, and retried by nothing -- the sweep that pulls it
	// back in by the attempt is the only thing that ever will, and a
	// build that deleted the sweep kept every other test green.
	if err := h.srv.reconcile(t.Context()); err != nil {
		t.Fatalf("startup reconciliation: %v", err)
	}

	if got := len(h.ready()); got != 1 {
		t.Errorf("ready attempts = %d; want 1 — startup did not find the "+
			"attempt a crash between the two commits left behind", got)
	}
}

// RecordEvidence must reject values outside the monotonic evidence model.
func TestRecordEvidenceClassifiesEveryOutcome(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-observe", "app", 53)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-observe")

	record := func(id string, e store.Evidence) error {
		return h.store.Tx(t.Context(), func(tx *store.Tx) error {
			return tx.RecordEvidenceForLease(assignment.LeaseID(id), e)
		})
	}

	if err := record(string(lease.ID), store.Evidence("definitely-not-a-value")); !errors.Is(err, store.ErrInvalidExecutionObservation) {
		t.Errorf("an unknown value returned %v; want ErrInvalidExecutionObservation — "+
			"a silent no-op tells the caller something was recorded when nothing was", err)
	}
	if err := record("no-such-lease", store.EvidenceRunningObserved); err == nil {
		t.Error("recording against a missing lease succeeded; the target's absence was swallowed")
	}

	if err := record(string(lease.ID), store.EvidenceRunningObserved); err != nil {
		t.Fatalf("recording a running observation: %v", err)
	}
	// Re-observing the same fact is not a fault.
	if err := record(string(lease.ID), store.EvidenceRunningObserved); err != nil {
		t.Errorf("repeating an identical observation returned %v; want explicit idempotent success", err)
	}
	// A slower writer cannot unmake an observation.
	if err := record(string(lease.ID), store.EvidenceRuntimePrepared); !errors.Is(err, store.ErrObservationConflict) {
		t.Errorf("a backwards write returned %v; want ErrObservationConflict — a runner "+
			"that was seen running cannot become one that never started", err)
	}
	if got := h.attemptByLease(lease.ID); got.Evidence != store.EvidenceRunningObserved {
		t.Errorf("evidence = %s after the rejected writes; want running_observed untouched", got.Evidence)
	}
}

// TestRedeliveryToleratesADifferentAcquisition is the binding-wedge guard.
// A message's availables are acquired by a separate call whose result
// depends on who else asked, so two deliveries of one message id can
// legitimately carry different acquired sets. Folding those into the
// fingerprint made that read as the provider changing a payload under a
// stable key: RecordDelivery returned contract drift, persistDelivery
// failed, the message was never acknowledged, and because the queue is
// ordered nothing behind it was ever served again.
func TestRedeliveryToleratesADifferentAcquisition(t *testing.T) {
	h := newHarness(t, 2)

	// First arrival: the broker assigned one job and the session acquired
	// one available.
	first := &githubactions.Message{
		ID:       7,
		Assigned: []assignment.WorkloadAssignment{demand("job-assigned", "app", 80)},
		Acquired: []assignment.WorkloadAssignment{demand("job-acquired-a", "app", 81)},
	}
	if _, err := h.srv.persistDelivery(t.Context(), h.bind, first); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// Redelivery of the same message id: the acknowledgement was lost, and
	// this time the acquisition granted a different job.
	second := &githubactions.Message{
		ID:       7,
		Assigned: []assignment.WorkloadAssignment{demand("job-assigned", "app", 80)},
		Acquired: []assignment.WorkloadAssignment{demand("job-acquired-b", "app", 82)},
	}
	if _, err := h.srv.persistDelivery(t.Context(), h.bind, second); err != nil {
		t.Fatalf("a redelivery with a different acquisition wedged the binding: %v", err)
	}

	// The broker's own payload is still what identity is checked against,
	// so a message id whose assigned set changed is still refused.
	drifted := &githubactions.Message{
		ID:       7,
		Assigned: []assignment.WorkloadAssignment{demand("job-different", "app", 99)},
	}
	if _, err := h.srv.persistDelivery(t.Context(), h.bind, drifted); err == nil {
		t.Error("a changed assigned set was accepted; contract drift is no longer detected")
	}
}

// TestAWedgedRedeliveryStillLandsItsHints. A message the binding already
// acknowledged can come back anyway - the wedge the acknowledgement Warn
// describes - and the loop throttles rather than spinning on it. What the
// throttle must not do is skip the message's content: a cancellation hint
// riding in the redelivery has to land, or the wedged binding keeps
// serving, and burning a capsule on, a job the provider already cancelled.
func TestAWedgedRedeliveryStillLandsItsHints(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Millisecond

	// One lane, already busy: the loop schedules before it receives, and
	// a free lane would lease the attempt before the hint could reach it.
	// A cancellation matters exactly while the attempt is still waiting.
	// The credit is taken through the allocator, which is what the
	// scheduler actually consults.
	if !h.srv.alloc.TryReserve(h.bind.key) {
		t.Fatal("the lane was never occupied")
	}

	// A confirmed delivery: recorded, acknowledged, cursor past it.
	msg := &githubactions.Message{
		ID:       41,
		Assigned: []assignment.WorkloadAssignment{demand("job-cancelled-upstream", "app", 41)},
	}
	delivery, err := h.srv.persistDelivery(t.Context(), h.bind, msg)
	if err != nil {
		t.Fatal(err)
	}
	h.inStore(func(tx *store.Tx) error {
		if _, err := tx.AckRequested(assignment.DeliveryID(delivery)); err != nil {
			return err
		}
		return tx.AckConfirmed(assignment.DeliveryID(delivery))
	})

	// The broker sends it again, now carrying the cancellation. One full
	// loop iteration: the session hands over exactly this message, then
	// blocks.
	msg.Completed = []assignment.WorkloadLifecycleEvent{{
		Kind: assignment.LifecycleCompleted, SourceWorkloadKey: "job-cancelled-upstream",
		Result: "canceled",
	}}
	redelivery := &replaySession{msg: msg, drained: make(chan struct{})}
	h.bind.session = redelivery

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()
	select {
	case <-redelivery.drained:
	case <-time.After(10 * time.Second):
		t.Error("the loop never consumed the redelivered message")
	}
	cancel()
	<-done

	// The state, not the queue: an attempt can also leave ready by being
	// leased, which is exactly the outcome the hint exists to prevent.
	var attempt store.Attempt
	h.inStore(func(tx *store.Tx) error {
		open, err := tx.OpenAttemptByWorkload(h.bind.bindingID, "job-cancelled-upstream")
		if err == nil {
			attempt = open
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// Settled, then: fetch it through its delivery.
		attempts, aerr := tx.AttemptsOfDelivery(assignment.DeliveryID(delivery))
		if aerr != nil {
			return aerr
		}
		attempt = attempts[0]
		return nil
	})
	if attempt.State != store.AttemptCanceled || attempt.Resolution != assignment.ResolutionRemoteCanceled {
		t.Errorf("the attempt is %q/%q; want canceled/remote_canceled - the hint in the "+
			"wedged redelivery never landed, and the binding will burn a capsule on a "+
			"job the provider already cancelled", attempt.State, attempt.Resolution)
	}
}

// replaySession hands over one message once, then blocks: the shape of a
// broker stuck redelivering something already acknowledged.
type replaySession struct {
	mu      sync.Mutex
	msg     *githubactions.Message
	served  bool
	drained chan struct{}
}

func (s *replaySession) Receive(ctx context.Context) (*githubactions.Message, error) {
	s.mu.Lock()
	first := !s.served
	s.served = true
	s.mu.Unlock()
	if first {
		return s.msg, nil
	}
	close(s.drained)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *replaySession) Acknowledge(context.Context, int) error { return nil }
func (s *replaySession) SetCapacity(int)                        {}
func (s *replaySession) Initial() *githubactions.Statistics     { return nil }
func (s *replaySession) Close(context.Context) error            { return nil }

// cancellingSession hands over one message and cancels the loop from
// inside the acknowledgement — the shape of a shutdown that lands
// exactly there. It is the worst honest moment: the message is gone from
// the broker's queue and this process is stopping.
type cancellingSession struct {
	msg    *githubactions.Message
	cancel context.CancelFunc

	mu     sync.Mutex
	served bool
}

func (s *cancellingSession) Receive(ctx context.Context) (*githubactions.Message, error) {
	s.mu.Lock()
	first := !s.served
	s.served = true
	s.mu.Unlock()
	if first {
		return s.msg, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *cancellingSession) Acknowledge(context.Context, int) error {
	s.cancel()
	return nil
}
func (s *cancellingSession) SetCapacity(int)                    {}
func (s *cancellingSession) Initial() *githubactions.Statistics { return nil }
func (s *cancellingSession) Close(context.Context) error        { return nil }

// TestACancellationIsDurableBeforeTheMessageIsAcknowledged: everything a
// message carries is written down before the message is given up.
//
// An acknowledged message is never sent again, and nothing re-derives a
// cancellation from the provider — so one lost between the
// acknowledgement and the write is lost for good, and the workload it
// closed goes on to spend a whole capsule on work the provider already
// finished with. Recorded first, a shutdown in that window leaves the
// message unacknowledged and the broker redelivers it.
func TestACancellationIsDurableBeforeTheMessageIsAcknowledged(t *testing.T) {
	h := newHarness(t, 1)

	// One message carrying the assignment and the cancellation that
	// closes it — the shape the provider actually sends when a run is
	// cancelled while its jobs are being handed out. Delivering the two
	// separately would not reach this: the loop drains ready work at the
	// top of every turn, so the attempt is leased before any later
	// message can close it, and a cancellation correctly refuses to
	// touch a serving attempt.
	ctx, cancel := context.WithCancel(t.Context())
	msg := &githubactions.Message{
		ID:       901,
		Assigned: []assignment.WorkloadAssignment{demand("job-closed", "app", 44)},
		Completed: []assignment.WorkloadLifecycleEvent{{
			Kind: assignment.LifecycleCompleted, SourceWorkloadKey: "job-closed",
			Result: "canceled",
		}},
	}
	h.bind.session = &cancellingSession{msg: msg, cancel: cancel}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the loop never returned after its context was cancelled")
	}

	// The state, not the queue: an attempt also leaves the ready list by
	// being leased, which is the very outcome the cancellation exists to
	// prevent.
	var got store.Attempt
	h.inStore(func(tx *store.Tx) error {
		open, err := tx.OpenAttemptByWorkload(h.bind.bindingID, "job-closed")
		if err == nil {
			got = open
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		attempts, err := tx.AttemptsOfDelivery(1)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if a.SourceWorkloadKey == "job-closed" {
				got = a
			}
		}
		return nil
	})
	if got.State != store.AttemptCanceled {
		t.Errorf("the cancelled workload is %q; want canceled — the cancellation was lost "+
			"with the message that carried it", got.State)
	}
}

// TestAnUnrecordedLifecycleEventIsReported: the message is acknowledged
// on the strength of everything it carried being written down, and an
// acknowledged message is never sent again -- nothing re-derives a
// cancellation from the provider. So a record that fails has to be
// reported rather than logged and stepped over, which is what left a
// cancelled workload ready to lease and a whole capsule spent on work
// the provider had already closed.
func TestAnUnrecordedLifecycleEventIsReported(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-unrecorded", "app", 93)); err != nil {
		t.Fatal(err)
	}

	// A context that is already done: the record opens a transaction on
	// it, which is the failure this reports rather than swallows.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	msg := &githubactions.Message{
		ID: 993,
		Completed: []assignment.WorkloadLifecycleEvent{{
			Kind: assignment.LifecycleCompleted, SourceWorkloadKey: "job-unrecorded",
			Result: "canceled",
		}},
	}
	if err := h.srv.recordLifecycleEvents(ctx, h.bind, msg); err == nil {
		t.Fatal("a lifecycle event that could not be recorded reported success; " +
			"the message that carried it would be acknowledged and never sent again")
	}
}

// TestALostLeaseClaimReturnsItsCredit: the reserve and the lease commit
// are two steps, and losing the second is ordinary — two passes racing
// one ready attempt is the reason the claim is a compare-and-swap. A
// credit reserved for a lease that was never created has to come back,
// or every lost race burns one permanently: a tier at parallelism four
// decays to zero over a day of contention, and nothing reports why.
func TestALostLeaseClaimReturnsItsCredit(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-raced", "app", 95)); err != nil {
		t.Fatal(err)
	}
	// The race, made deterministic: the pass read the attempt as ready,
	// and the other pass wins the claim before this one reserves for it,
	// so the compare-and-swap refuses.
	var raced store.Attempt
	h.inStore(func(tx *store.Tx) error {
		ready, err := tx.ReadyAttempts(h.bind.bindingID)
		if err != nil {
			return err
		}
		raced = ready[0]
		return nil
	})
	leaseFor(t, h, "job-raced")

	if !h.srv.admit(t.Context(), h.bind, raced) {
		t.Fatal("a lost claim reported admission full; the pass would stop instead of continuing")
	}
	if got := h.srv.alloc.Active(h.bind.key); got != 0 {
		t.Errorf("active credits = %d after a claim this pass lost; want 0 — "+
			"the credit is burned for the life of the process", got)
	}
	if !h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("the pool refused a reserve after the lost claim; the credit did not come back")
	}
	h.srv.alloc.Release(h.bind.key)
}

// TestAFailedAcknowledgementStaysRetryable: the broker took the message
// and the confirmation could not be delivered — an outcome that is
// uncertain, not final. Recorded as confirmed, the broker's redelivery
// of the same message reads as one this binding already acknowledged,
// which nothing retries: the queue is ordered, so the binding stops
// serving permanently, one warn line per poll.
func TestAFailedAcknowledgementStaysRetryable(t *testing.T) {
	h := newHarness(t, 1)
	var delivery assignment.DeliveryID
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		delivery, err = tx.RecordDelivery(h.bind.bindingID,
			assignment.DeliveryKey(h.bind.scaleSetID, 701), [32]byte{}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.bind.session = &stubSession{ackErr: errors.New("broker unreachable")}

	if h.srv.acknowledgeDelivery(t.Context(), h.bind, delivery, 701) {
		t.Fatal("a failed acknowledgement reported the cursor advanced")
	}

	var proceed bool
	h.inStore(func(tx *store.Tx) error {
		var err error
		proceed, err = tx.AckRequested(delivery)
		return err
	})
	if !proceed {
		t.Error("the delivery reads as already acknowledged; the broker's redelivery will " +
			"never be re-acknowledged and the binding stops advancing")
	}
}
