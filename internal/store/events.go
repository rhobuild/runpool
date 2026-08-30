package store

import "slices"

// EventKind is what an attempt_events row records. The twelve values
// are closed by the column and were spelled as literals in four
// packages, with no list anywhere -- so a kind the schema does not admit
// failed at the write, in a delivery path, at the moment the trail was
// being written for an operator to read.
//
// The type also holds an ordering the compiler could not see.
// RecordEvent takes the idempotency key before the kind; the controller
// wraps it in a closure that takes them the other way round. Both are
// correct today, and an edit that made either match the other would
// have swapped every event's kind with its key.
type EventKind string

const (
	// EventAttemptCreated: a delivery became durable work.
	EventAttemptCreated EventKind = "attempt_created"
	// EventLeaseAttached: an attempt was claimed by a lease.
	EventLeaseAttached EventKind = "lease_attached"
	// EventRuntimePrepared: a capsule was built and not yet started.
	EventRuntimePrepared EventKind = "runtime_prepared"
	// EventStartAuthorized: the start was handed over. Past this line
	// the work may have run, and nothing may assume it did not.
	EventStartAuthorized EventKind = "execution_start_authorized"
	// EventRunningObserved: the runtime was seen running.
	EventRunningObserved EventKind = "running_observed"
	// EventExitObserved: the runtime was seen to exit.
	EventExitObserved EventKind = "exit_observed"
	// EventCleanupCompleted is the serving's finalizing transaction:
	// the disposition commits with it. There is no started event -- the
	// trail rules on evidence, never on how far cleanup got.
	EventCleanupCompleted EventKind = "cleanup_completed"
	// EventManualReviewRequested: the attempt is waiting for a person.
	EventManualReviewRequested EventKind = "manual_review_requested"
	// EventOperatorResolved: a person ended that wait.
	EventOperatorResolved EventKind = "operator_resolved"
	// EventAttemptSettled: the attempt reached its resolution.
	EventAttemptSettled EventKind = "attempt_settled"
	// EventAttemptSuperseded: a newer delivery replaced it.
	EventAttemptSuperseded EventKind = "attempt_superseded"
	// EventRemoteCanceled: the provider called the workload off. It
	// shares a spelling with assignment.ResolutionRemoteCanceled and is
	// a different vocabulary: one is what happened, the other is what
	// was decided about it.
	EventRemoteCanceled EventKind = "remote_canceled"
)

var eventKinds = []EventKind{
	EventAttemptCreated,
	EventLeaseAttached,
	EventRuntimePrepared,
	EventStartAuthorized,
	EventRunningObserved,
	EventExitObserved,
	EventCleanupCompleted,
	EventManualReviewRequested,
	EventOperatorResolved,
	EventAttemptSettled,
	EventAttemptSuperseded,
	EventRemoteCanceled,
}

// EventKinds returns every persisted kind the attempt trail can hold.
func EventKinds() []EventKind { return slices.Clone(eventKinds) }
