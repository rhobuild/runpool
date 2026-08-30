package store

import (
	"errors"
	"fmt"
	"slices"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Evidence is what is durably known about whether an attempt's workload
// executed. It belongs to the attempt, never to the lease: a lease state
// describes cleanup, and reading cleanup as execution is how a job that
// never ran gets settled as if it had.
//
// Every value names an observation, never an inference, and the machine
// is strictly monotonic: not_started is where an attempt begins, and
// each later value is written only when the thing it names was seen to
// happen. There is no value for "could not observe" — an unobservable
// outcome is an operational condition, recorded as a manual-review
// reason, not a rung that could displace or be displaced by something
// that was actually seen.
type Evidence string

const (
	// EvidenceNotStarted means nothing has been prepared.
	EvidenceNotStarted Evidence = "not_started"
	// EvidenceRuntimePrepared means the capsule exists — resources
	// created, credential delivered — and no start has been authorized,
	// so the delivery is not consumed and a retry is safe.
	EvidenceRuntimePrepared Evidence = "runtime_prepared"
	// EvidenceStartAuthorized means the controller committed to asking
	// the runtime to start. From here the outcome is decided by
	// observation: at-most-once wins, and a job with external effects is
	// never re-run on a maybe.
	EvidenceStartAuthorized Evidence = "execution_start_authorized"
	// EvidenceRunningObserved means the daemon showed the runtime
	// running.
	EvidenceRunningObserved Evidence = "running_observed"
	// EvidenceExitObserved means the daemon showed the runtime exited.
	EvidenceExitObserved Evidence = "exit_observed"
)

// ErrInvalidExecutionObservation reports a value outside the vocabulary.
// It is an error rather than a silent no-op: an unrecognised observation
// that reports success leaves the caller believing something was
// recorded.
var ErrInvalidExecutionObservation = errors.New("execution observation is not a known value")

// ErrObservationConflict reports a write that would contradict what is
// already recorded — moving backwards, or racing another writer. The
// caller re-reads and decides; the store never resolves the conflict by
// overwriting an observation.
var ErrObservationConflict = errors.New("execution observation conflicts with what is recorded")

// evidenceStates contains every state the monotonic machine can reach, in the
// order it can reach them. The order is the rule: evidence only ever
// moves forward, and a write that would move it back is refused.
//
// This is the source and the rank below is derived from it, not the
// other way round. Building the slice by indexing the map assumed the
// ranks were a contiguous run with no duplicates and no gaps -- true of
// the data, stated by nothing, and both ways of breaking it are bad: a
// gap panics in this package's initializer, which takes down every
// binary that imports the store at startup, and a duplicate leaves a
// hole in the slice that only a test happens to catch. Written this way
// neither is expressible, and the hand-assigned numbers are gone with
// their own chance to disagree.
var evidenceStates = []Evidence{
	EvidenceNotStarted,
	EvidenceRuntimePrepared,
	EvidenceStartAuthorized,
	EvidenceRunningObserved,
	EvidenceExitObserved,
}

// EvidenceStates returns the monotonic evidence vocabulary in order.
func EvidenceStates() []Evidence { return slices.Clone(evidenceStates) }

// evidenceRank orders the vocabulary for the monotonic rule.
var evidenceRank = func() map[Evidence]int {
	m := make(map[Evidence]int, len(evidenceStates))
	for i, e := range evidenceStates {
		m[e] = i
	}
	return m
}()

// Valid reports whether e is part of the vocabulary.
func (e Evidence) Valid() bool {
	_, ok := evidenceRank[e]
	return ok
}

// Retriable reports whether the evidence permits an automatic retry.
// Only work that demonstrably never had a start authorized may be
// requeued; everything else is settled, adopted or held for a human.
func (e Evidence) Retriable() bool {
	return e == EvidenceNotStarted || e == EvidenceRuntimePrepared
}

// ReviewReason is why an attempt is held. An attempt in manual review is
// neither servable nor settled: it is visible, aged and waiting for a
// person, which is the honest shape of "we cannot know".
//
// It is a named type because it travels as free text beside another free
// text -- the actor resolving the hold -- through three packages, and the
// compiler cannot see a transposition that files an operator's name as
// the reason a job is held.
type ReviewReason string

const (
	// ReviewReasonStartOutcomeUnknown: a start was authorized and the
	// runtime could not be observed afterwards.
	ReviewReasonStartOutcomeUnknown ReviewReason = "start_outcome_unknown"
	// ReviewReasonRetryBudgetExhausted: the work provably never began,
	// again, for as many servings as the budget allows. Nothing here is
	// unsafe to retry; what is unproven is whether retrying will ever
	// stop, and that is a question for an operator.
	ReviewReasonRetryBudgetExhausted ReviewReason = "retry_budget_exhausted"
	// ReviewReasonIncompatibleCapsule: the image this tier launches does
	// not speak the controller's control protocol. The work never began,
	// so retrying is safe — and pointless, because the next attempt
	// launches the same image. Holding it names the one thing that has to
	// change, instead of spending the retry budget discovering it three
	// times per job.
	ReviewReasonIncompatibleCapsule ReviewReason = "capsule_incompatible"
)

var reviewReasons = []ReviewReason{
	ReviewReasonStartOutcomeUnknown,
	ReviewReasonRetryBudgetExhausted,
	ReviewReasonIncompatibleCapsule,
}

// ReviewReasons returns every persisted reason an attempt can be held.
func ReviewReasons() []ReviewReason { return slices.Clone(reviewReasons) }

// RecordEvidence advances what is known about an attempt's execution and
// classifies every outcome instead of collapsing them into success:
//
//   - the observation was applied — nil;
//   - the same observation is already recorded — nil, explicitly
//     idempotent, because re-observing a fact is not a fault;
//   - the write would move backwards — ErrObservationConflict, since an
//     observation that was made cannot be unmade by a slower writer;
//   - a concurrent writer moved the row between read and write —
//     ErrObservationConflict, and the caller re-reads;
//   - the attempt does not exist — ErrNotFound;
//   - the value is outside the vocabulary —
//     ErrInvalidExecutionObservation.
//
// A silent no-op is the one outcome this must never produce: the caller
// would believe something was recorded when nothing was.
func (t *Tx) RecordEvidence(attemptID assignment.AttemptID, e Evidence) error {
	if !e.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidExecutionObservation, string(e))
	}
	attempt, err := t.Get(attemptID)
	if err != nil {
		return err
	}
	current := attempt.Evidence
	if !current.Valid() {
		// A row holding a value outside the vocabulary is corruption,
		// not a base to build on.
		return fmt.Errorf("%w: attempt %s holds unrecognised evidence %q",
			ErrInvalidExecutionObservation, attemptID, string(current))
	}
	switch {
	case e == current:
		return nil // already recorded; re-observing a fact is idempotent
	case evidenceRank[e] < evidenceRank[current]:
		return fmt.Errorf("%w: %s cannot replace %s on attempt %s",
			ErrObservationConflict, e, current, attemptID)
	}
	// Compare-and-swap on the value just read: if another writer moved
	// the row in between, nothing is written and the conflict is
	// reported rather than resolved by overwriting.
	affected, err := t.q.RecordAttemptEvidence(t.ctx, sqlitedb.RecordAttemptEvidenceParams{
		Next: string(e), AttemptID: string(attemptID), Current: string(current),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s moved while recording %s",
			ErrObservationConflict, attemptID, e)
	}
	// The advance goes in the trail too, with a time. Evidence is a
	// high-water mark and says only where an attempt got, never when --
	// so an attempt held because a start was authorized and its runtime
	// could not be observed showed an operator a trail that jumped from
	// lease_attached to the hold, with the authorization that caused it
	// durable nowhere but a log that rotates.
	//
	// Here rather than at the call sites: every caller is already inside
	// this transaction, so the event and the column can never disagree,
	// and an advance to a rung happens at most once per attempt -- the
	// equal case returned above, backwards errored, and a racing loser
	// rolls this back with it. That is what makes the key unique.
	return t.RecordEventDetail(attemptID, "evidence:"+string(e), eventKindOf(e),
		map[string]string{"observer": "controller"})
}

// eventKindOf names the trail entry for an evidence advance. Only the
// rungs above the floor appear: nothing advances to not_started, which
// is where every attempt begins.
func eventKindOf(e Evidence) EventKind {
	switch e {
	case EvidenceRuntimePrepared:
		return EventRuntimePrepared
	case EvidenceStartAuthorized:
		return EventStartAuthorized
	case EvidenceRunningObserved:
		return EventRunningObserved
	case EvidenceExitObserved:
		return EventExitObserved
	}
	return ""
}

// RecordEvidenceForLease advances the evidence of the attempt a lease is
// serving. The runtime paths hold a lease and observe the capsule; the
// record they are updating is the attempt's.
func (t *Tx) RecordEvidenceForLease(leaseID assignment.LeaseID, e Evidence) error {
	lease, err := t.LeaseByID(leaseID)
	if err != nil {
		return err
	}
	return t.RecordEvidence(lease.AttemptID, e)
}
