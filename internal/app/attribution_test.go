package app

import (
	"context"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

// nameRuntime attaches a runner name to a lease, the way provisioning
// does once the capsule's runner is registered.
func nameRuntime(t *testing.T, h *harness, lease store.Lease, runtimeName string) {
	t.Helper()
	if err := h.store.Tx(context.Background(), func(tx *store.Tx) error {
		return tx.SetLeaseRuntimeName(lease.ID, assignment.RuntimeName(runtimeName))
	}); err != nil {
		t.Fatal(err)
	}
}

// eventsOf lists an attempt's recorded lifecycle.
func eventsOf(t *testing.T, h *harness, attemptID assignment.AttemptID) []string {
	t.Helper()
	var kinds []string
	if err := h.store.Tx(context.Background(), func(tx *store.Tx) error {
		events, err := tx.Events(attemptID)
		if err != nil {
			return err
		}
		for _, e := range events {
			kinds = append(kinds, e.Kind)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return kinds
}

func contains(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// TestExecutionIsRecordedAgainstTheWorkloadThatRan is the correctness of
// the evidence model.
//
// A runner is minted against a scale set, not a job, so the provider can
// hand a runner this instance provisioned for one workload a different
// one entirely — which is exactly what happens when an assignment lapses
// and its job is requeued under a new identity. Correlating the
// observation by the runner's name answers "which attempt was this runner
// provisioned for", and records the execution against an attempt that
// never ran.
//
// The event names its own workload. That is the attempt whose evidence
// this is.
func TestExecutionIsRecordedAgainstTheWorkloadThatRan(t *testing.T) {
	h := newHarness(t, 2)

	if err := h.deliver(demand("job-lapsed", "app", 70)); err != nil {
		t.Fatal(err)
	}
	if err := h.deliver(demand("job-requeued", "app", 70)); err != nil {
		t.Fatal(err)
	}

	// The runner exists for the first workload; the second is still
	// waiting. This is the shape a requeue leaves behind.
	lapsedLease, lapsedAttempt := leaseFor(t, h, "job-lapsed")
	nameRuntime(t, h, lapsedLease, "runpool-ghost")

	var requeuedAttempt string
	for _, a := range h.ready() {
		if a.SourceWorkloadKey == "job-requeued" {
			requeuedAttempt = string(a.ID)
		}
	}
	if requeuedAttempt == "" {
		t.Fatal("the requeued workload has no attempt to be evidence for")
	}

	// The provider reports the requeued workload running, on the runner
	// minted for the lapsed one.
	h.srv.recordLifecycleEvents(t.Context(), h.bind, &githubactions.Message{
		ID: 901,
		Started: []assignment.WorkloadLifecycleEvent{{
			SourceWorkloadKey: "job-requeued",
			RuntimeName:       "runpool-ghost",
		}},
	})

	if !contains(eventsOf(t, h, assignment.AttemptID(requeuedAttempt)), "running_observed") {
		t.Error("the workload that ran has no record of running; its evidence went elsewhere")
	}
	if contains(eventsOf(t, h, lapsedAttempt), "running_observed") {
		t.Error("the lapsed workload is recorded as running; it never ran, and its " +
			"attempt now claims an execution that belongs to another job")
	}
}

// TestAnObservationWithNoAttemptFallsBackToTheRunner. A workload this
// instance never recorded, or whose attempt already settled, still has a
// runner — and the observation is worth keeping against the attempt that
// runner serves rather than dropped.
func TestAnObservationWithNoAttemptFallsBackToTheRunner(t *testing.T) {
	h := newHarness(t, 1)
	if err := h.deliver(demand("job-known", "app", 71)); err != nil {
		t.Fatal(err)
	}
	lease, attemptID := leaseFor(t, h, "job-known")
	nameRuntime(t, h, lease, "runpool-known")

	h.srv.recordLifecycleEvents(t.Context(), h.bind, &githubactions.Message{
		ID: 902,
		Started: []assignment.WorkloadLifecycleEvent{{
			SourceWorkloadKey: "job-never-seen",
			RuntimeName:       "runpool-known",
		}},
	})

	if !contains(eventsOf(t, h, attemptID), "running_observed") {
		t.Error("an observation naming an unknown workload was dropped; the runner it " +
			"names is this instance's, and the attempt it serves is the best answer left")
	}
}

// TestALateObservationStaysWithTheAttemptThatRanIt is the other half of
// the correlation rule, and the case where answering by workload alone
// gets it wrong.
//
// A requeue delivers the same workload again, so the delivery path
// supersedes the open attempt and opens a successor for it. Both attempts
// name that one workload, and only the first one ever had a runner. An
// observation of that first run can still arrive afterwards — a session
// replaced mid-flight replays every message it never acknowledged — and
// answering "which attempt is open for this workload" hands it the
// successor, which has started nothing.
//
// The runner is what tells them apart: its lease belongs to an attempt
// for this same workload, so the report is of that attempt's run.
func TestALateObservationStaysWithTheAttemptThatRanIt(t *testing.T) {
	h := newHarness(t, 2)

	if err := h.deliver(demand("job-requeued", "app", 70)); err != nil {
		t.Fatal(err)
	}
	firstLease, firstAttempt := leaseFor(t, h, "job-requeued")
	nameRuntime(t, h, firstLease, "runpool-first")
	driveLeaseTo(t, h, firstLease.ID, store.LeaseReleased)

	// The requeue: the same workload arrives again, superseding the
	// attempt that holds the runner.
	if err := h.deliver(demand("job-requeued", "app", 70)); err != nil {
		t.Fatal(err)
	}
	var successor string
	for _, a := range h.ready() {
		if a.SourceWorkloadKey == "job-requeued" {
			successor = string(a.ID)
		}
	}
	if successor == "" || assignment.AttemptID(successor) == firstAttempt {
		t.Fatalf("the requeue left no successor attempt (got %q against the first %q); "+
			"this case needs two attempts for one workload", successor, firstAttempt)
	}

	// The delayed report of the first run.
	h.srv.recordLifecycleEvents(t.Context(), h.bind, &githubactions.Message{
		ID: 902,
		Completed: []assignment.WorkloadLifecycleEvent{{
			SourceWorkloadKey: "job-requeued",
			RuntimeName:       "runpool-first", Result: "succeeded",
		}},
	})

	if !contains(eventsOf(t, h, firstAttempt), "exit_observed") {
		t.Error("the attempt whose runner produced the observation has no record of it")
	}
	if contains(eventsOf(t, h, assignment.AttemptID(successor)), "exit_observed") {
		t.Error("the successor is recorded as having exited; it holds no lease and has " +
			"started nothing, so an operator reading its trail is told a run happened")
	}
}

// TestALateCancellationDoesNotCloseTheSuccessor. A cancellation is an
// observation like any other, and it has to reach the attempt the
// observation was correlated to. Resolving the workload's open attempt on
// its own gives a second, independent answer, and after a requeue that
// answer is the successor: the run being cancelled is the one that came
// before it, and closing the successor destroys work the provider has
// just handed to this instance and is still waiting on.
func TestALateCancellationDoesNotCloseTheSuccessor(t *testing.T) {
	h := newHarness(t, 2)

	if err := h.deliver(demand("job-requeued", "app", 70)); err != nil {
		t.Fatal(err)
	}
	firstLease, firstAttempt := leaseFor(t, h, "job-requeued")
	nameRuntime(t, h, firstLease, "runpool-first")
	driveLeaseTo(t, h, firstLease.ID, store.LeaseReleased)

	if err := h.deliver(demand("job-requeued", "app", 70)); err != nil {
		t.Fatal(err)
	}
	var successor string
	for _, a := range h.ready() {
		if a.SourceWorkloadKey == "job-requeued" {
			successor = string(a.ID)
		}
	}
	if successor == "" || assignment.AttemptID(successor) == firstAttempt {
		t.Fatalf("the requeue left no successor attempt (got %q against the first %q)",
			successor, firstAttempt)
	}

	// The provider cancels the run that preceded the requeue.
	h.srv.recordLifecycleEvents(t.Context(), h.bind, &githubactions.Message{
		ID: 903,
		Completed: []assignment.WorkloadLifecycleEvent{{
			SourceWorkloadKey: "job-requeued",
			RuntimeName:       "runpool-first", Result: "canceled", Canceled: true,
		}},
	})

	if !contains(eventsOf(t, h, firstAttempt), "remote_canceled") {
		t.Error("the cancellation never reached the attempt whose run it ends; " +
			"dropping late cancellations entirely would also leave the successor alone")
	}
	var after store.Attempt
	h.inStore(func(tx *store.Tx) error {
		var err error
		after, err = tx.Get(assignment.AttemptID(successor))
		return err
	})
	if after.State != store.AttemptReady {
		t.Errorf("the successor is %q/%q; it was never cancelled, and closing it "+
			"destroys work the provider is still waiting for a runner on",
			after.State, after.Resolution)
	}
}

// TestASecondServingsHintsAreNotSwallowed. attempt_events dedupes on an
// idempotency key, and the provider's lifecycle hints used a fixed one
// per attempt. An attempt is served once per runtime now, so the second
// serving's hints read as replays of the first and vanished - the trail
// showed one start and one exit stitched across two different runners.
func TestASecondServingsHintsAreNotSwallowed(t *testing.T) {
	h := newHarness(t, 2)
	if err := h.deliver(demand("job-retried", "app", 70)); err != nil {
		t.Fatal(err)
	}
	firstLease, attemptID := leaseFor(t, h, "job-retried")
	nameRuntime(t, h, firstLease, "runpool-first")

	hint := func(runtime assignment.RuntimeName) {
		h.srv.recordLifecycleEvents(t.Context(), h.bind, &githubactions.Message{
			ID: 910,
			Started: []assignment.WorkloadLifecycleEvent{{
				SourceWorkloadKey: "job-retried",
				RuntimeName:       runtime,
			}},
		})
	}
	hint("runpool-first")

	// The first serving ends without proof and the attempt is served
	// again by a new runtime.
	driveLeaseTo(t, h, firstLease.ID, store.LeaseReleased)
	h.inStore(func(tx *store.Tx) error { return tx.Requeue(attemptID) })
	secondLease, _ := leaseFor(t, h, "job-retried")
	nameRuntime(t, h, secondLease, "runpool-second")
	hint("runpool-second")

	events := eventsOf(t, h, attemptID)
	got := 0
	for _, kind := range events {
		if kind == "running_observed" {
			got++
		}
	}
	if got != 2 {
		t.Errorf("running_observed recorded %d times for two servings; want 2 - a "+
			"fixed idempotency key swallows the second serving's hint as a replay "+
			"of the first (trail: %v)", got, events)
	}
}
