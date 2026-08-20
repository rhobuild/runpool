package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"

	"github.com/rhobuild/runpool/internal/assignment"
)

// AttemptState is where an attempt stands in the walk from queued to
// resolved.
//
// It is a type for the reason LeaseState is one. These values are
// compared against literals in guards spread across three packages, and
// every one of those must agree with a list written in SQL — where a
// typo is not a compile error but a guard that matches nothing and
// reports success.
type AttemptState string

const (
	AttemptReady        AttemptState = "ready"
	AttemptLeased       AttemptState = "leased"
	AttemptPreparing    AttemptState = "preparing"
	AttemptPrepared     AttemptState = "prepared"
	AttemptStarting     AttemptState = "starting"
	AttemptRunning      AttemptState = "running"
	AttemptManualReview AttemptState = "manual_review"
	AttemptSuperseded   AttemptState = "superseded"
	AttemptSettled      AttemptState = "settled"
	AttemptCanceled     AttemptState = "canceled"
)

// AllAttemptStates lists every state an attempt can hold, terminal ones
// included, in the order the walk reaches them.
//
// It exists for the reason AllLeaseStates does. Anything that has to
// decide something for every state — a disposition, a report, a table
// test — needs the list to come from one place, or a state added later
// is quietly left undecided by whichever copy nobody updated.
// TestAttemptStatesCoverTheSchema holds it against the constraint the
// database enforces, so the list cannot drift from what a row may
// actually contain.
var AllAttemptStates = []AttemptState{
	AttemptReady, AttemptLeased, AttemptPreparing, AttemptPrepared,
	AttemptStarting, AttemptRunning, AttemptManualReview,
	AttemptSuperseded, AttemptSettled, AttemptCanceled,
}

// Attempt is the store's domain-facing view of one attempt row. It is a
// translation, not the generated row: consumers never see table types,
// so a schema change stops at this boundary.
type Attempt struct {
	ID                assignment.AttemptID
	DeliveryID        assignment.DeliveryID
	BindingID         assignment.BindingID
	SourceWorkloadKey assignment.SourceWorkloadKey
	TenantKey         string
	ProjectKey        string
	State             AttemptState
	Evidence          Evidence
	ReviewReason      string
	Resolution        string
	ReviewedBy        string
	ReceivedAt        int64
	ReviewedAt        int64
	SettledAt         int64
}

func fromRow(r sqlitedb.AssignmentAttempt) Attempt {
	// The one place a generated row becomes a domain value. Every
	// conversion below is here so no caller has to make one, which is
	// what keeps a swap outside this file a compile error.
	return Attempt{
		ID:                assignment.AttemptID(r.ID),
		DeliveryID:        assignment.DeliveryID(r.DeliveryID),
		BindingID:         assignment.BindingID(r.BindingID),
		SourceWorkloadKey: assignment.SourceWorkloadKey(r.SourceWorkloadKey),
		TenantKey:         r.TenantKey,
		ProjectKey:        r.ProjectKey,
		State:             AttemptState(r.State),
		Evidence:          Evidence(r.ExecutionEvidence),
		ReviewReason:      r.ReviewReason,
		Resolution:        r.Resolution,
		ReviewedBy:        r.ReviewedBy,
		ReceivedAt:        r.ReceivedAt,
		ReviewedAt:        r.ReviewedAt.Int64,
		SettledAt:         r.SettledAt.Int64,
	}
}

// Event is one lifecycle observation of an attempt, as an operator
// reads it.
type Event struct {
	Kind      string
	Detail    string
	CreatedAt int64
}

// Events lists an attempt's lifecycle, oldest first.
func (t *Tx) Events(attemptID assignment.AttemptID) ([]Event, error) {
	rows, err := t.q.ListAttemptEvents(t.ctx, string(attemptID))
	if err != nil {
		return nil, err
	}
	out := make([]Event, len(rows))
	for i, r := range rows {
		out[i] = Event{Kind: r.Kind, Detail: r.DetailJson, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

func fromRows(rows []sqlitedb.AssignmentAttempt) []Attempt {
	out := make([]Attempt, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out
}

// ReadyAttempts lists a binding's servable work, oldest first.
func (t *Tx) ReadyAttempts(bindingID assignment.BindingID) ([]Attempt, error) {
	rows, err := t.q.ListReadyAttempts(t.ctx, int64(bindingID))
	if err != nil {
		return nil, err
	}
	return fromRows(rows), nil
}

// ManualReviewAttempts lists everything held for a person, oldest first.
func (t *Tx) ManualReviewAttempts() ([]Attempt, error) {
	rows, err := t.q.ListManualReviewAttempts(t.ctx)
	if err != nil {
		return nil, err
	}
	return fromRows(rows), nil
}

// Get reloads one attempt by id, whatever state it moved to.
func (t *Tx) Get(attemptID assignment.AttemptID) (Attempt, error) {
	row, err := t.q.GetAttempt(t.ctx, string(attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	return fromRow(row), nil
}

// AttemptByLease resolves the attempt a lease is serving.
func (t *Tx) AttemptByLease(leaseID assignment.LeaseID) (Attempt, error) {
	row, err := t.q.GetAttemptByLease(t.ctx, string(leaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	return fromRow(row), nil
}

// AttemptsOfDelivery lists the attempts one delivery carries.
func (t *Tx) AttemptsOfDelivery(deliveryID assignment.DeliveryID) ([]Attempt, error) {
	rows, err := t.q.ListAttemptsByDelivery(t.ctx, int64(deliveryID))
	if err != nil {
		return nil, err
	}
	return fromRows(rows), nil
}

// StrandedAttempts finds attempts still attached to leases that already
// finished their own lifecycle — the crash window between releasing a
// lease and disposing of its attempt. Normally empty; every row is work
// nothing else will ever look at.
func (t *Tx) StrandedAttempts() ([]Attempt, error) {
	rows, err := t.q.ListAttemptsAttachedToReleasedLeases(t.ctx)
	if err != nil {
		return nil, err
	}
	return fromRows(rows), nil
}

// Advance walks the attempt state machine one edge, compare-and-swap.
// The walk is observability — disposition rests on evidence and the
// terminal transitions — so a conflict here reports rather than blocks.
func (t *Tx) Advance(attemptID assignment.AttemptID, from, to AttemptState) error {
	affected, err := t.q.TransitionAttempt(t.ctx, sqlitedb.TransitionAttemptParams{
		Next: string(to), AttemptID: string(attemptID), Current: string(from),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is no longer %s", ErrConflict, attemptID, from)
	}
	return nil
}

// AuthorizeStart is the compare-and-swap that decides whether a start may
// be attempted at all. It matches an attempt in a state this serving
// passed through, still held by this lease.
//
// Advance is the wrong shape here. Its edges are observability, written
// outside the transaction that matters, so an attempt can legitimately be
// behind by one when the start is authorized. Requiring the exact
// predecessor spends a prepared capsule and a serving on a write that
// only ever existed to be read by an operator.
//
// It takes the lease because the state set alone does not prove
// ownership. `prepared` proved it by accident — the starting goroutine
// wrote it itself moments earlier — while `leased` is written by whoever
// claimed the attempt. Naming the lease keeps at-most-once a property of
// the row rather than of three separate invariants agreeing.
func (t *Tx) AuthorizeStart(leaseID assignment.LeaseID, attemptID assignment.AttemptID) error {
	affected, err := t.q.AuthorizeAttemptStart(t.ctx, sqlitedb.AuthorizeAttemptStartParams{
		AttemptID: string(attemptID), LeaseID: string(leaseID),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is not lease %s's to start", ErrConflict, attemptID, leaseID)
	}
	return nil
}

// ErrRetryBudgetExhausted reports a requeue refused because the attempt
// has already had every serving it is allowed. It is not a safety
// refusal — the work provably never began — so the caller holds the
// attempt for review rather than settling it.
var ErrRetryBudgetExhausted = errors.New("attempt has used every serving its retry budget allows")

// DefaultRetryBudget is how many times one attempt may be leased before
// it is held for review.
//
// A serving that ends before the work begins is a transient: an image
// that would not pull, a daemon that would not answer. Retrying is right
// and it has to stop, because the failure that repeats is the one no
// number of retries fixes, and each one costs a capsule, a runner
// registration and a lane. Three matches the provider's own budget for an
// assignment nobody takes, so an attempt is not given up on before the
// provider itself would have.
//
// This breaks a loop rather than tunes a rate, which is why what a
// deployment may set instead is bounded narrowly, as a configuration
// rule. A count below one is not a smaller budget but no budget at all,
// which the store refuses on its own.
const DefaultRetryBudget = 3

// servingsSoFar is how many times this attempt has been leased. Every
// serving leaves its lease behind, so the history is the count — and the
// count is honest because retention refuses to prune a released lease
// whose attempt is still open (prunableReleasedLeases). GC forgetting
// those rows would silently reset this to zero and reopen the unbounded
// retry it exists to close.
func (t *Tx) servingsSoFar(attemptID assignment.AttemptID) (int, error) {
	var n int
	err := t.tx.QueryRow(
		`SELECT count(*) FROM capsule_leases WHERE attempt_id = ?`, attemptID).Scan(&n)
	return n, err
}

// withinRetryBudget refuses a requeue that would exceed the budget. It
// guards the automatic paths only: an operator resolving a review has
// established something the counter has not, and is not overruled by it.
func (t *Tx) withinRetryBudget(attemptID assignment.AttemptID) error {
	n, err := t.servingsSoFar(attemptID)
	if err != nil {
		return err
	}
	if n >= t.retryBudget {
		return fmt.Errorf("%w: attempt %s has been served %d times",
			ErrRetryBudgetExhausted, attemptID, n)
	}
	return nil
}

// Requeue returns an attempt whose work provably never began to the
// servable queue. Only leased, preparing and prepared qualify: they all
// precede the start authorization, and from starting onward
// at-most-once rules.
func (t *Tx) Requeue(attemptID assignment.AttemptID) error {
	if err := t.withinRetryBudget(attemptID); err != nil {
		return err
	}
	affected, err := t.q.RequeueAttempt(t.ctx, string(attemptID))
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is past the point of safe requeue", ErrConflict, attemptID)
	}
	return nil
}

// RequeueProvenInert is the one legal requeue past the start
// authorization: the daemon proved the start never took effect, so the
// at-most-once rule has nothing to protect.
func (t *Tx) RequeueProvenInert(attemptID assignment.AttemptID) error {
	if err := t.withinRetryBudget(attemptID); err != nil {
		return err
	}
	affected, err := t.q.RequeueProvenInertAttempt(t.ctx, string(attemptID))
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is not starting", ErrConflict, attemptID)
	}
	return nil
}

// CancelReady closes one named attempt on a remote cancellation, only
// while it is still ready. Anything past ready is running work, and
// refusing here is what keeps a cancellation aimed at unstarted work from
// touching a live capsule.
//
// It takes the attempt rather than the workload because a workload can
// hold more than one attempt over its life, and only the caller that
// correlated the event knows which of them the cancellation is about.
func (t *Tx) CancelReady(attemptID assignment.AttemptID, resolution string) error {
	affected, err := t.q.CancelReadyAttempt(t.ctx, sqlitedb.CancelReadyAttemptParams{
		Resolution: resolution, AttemptID: string(attemptID),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is not ready", ErrConflict, attemptID)
	}
	return t.RecordEvent(attemptID, "remote_canceled", "remote_canceled")
}

// OpenAttemptByWorkload resolves the unresolved attempt for a workload,
// or ErrNotFound when the workload has none open. A workload holds at
// most one open attempt at a time, which the partial unique index
// enforces, so the answer is unambiguous when there is one.
func (t *Tx) OpenAttemptByWorkload(bindingID assignment.BindingID, workloadKey assignment.SourceWorkloadKey) (Attempt, error) {
	row, err := t.q.GetOpenAttemptByWorkload(t.ctx, sqlitedb.GetOpenAttemptByWorkloadParams{
		BindingID: int64(bindingID), SourceWorkloadKey: string(workloadKey),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	return fromRow(row), nil
}

// CountReadyAttempts is how many attempts wait for admission on this
// binding. Reported so a queue that stops draining is visible before it
// is diagnosed.
func (t *Tx) CountReadyAttempts(bindingID assignment.BindingID) (int64, error) {
	return t.q.CountReadyAttempts(t.ctx, int64(bindingID))
}

// ResolveReviewToReady is the operator deciding a held attempt may run
// again — after verifying outside Runpool that the workload never
// executed, because the row itself holds no proof either way. The
// decision is audited: actor and timestamp on the row, actor and reason
// in the event.
func (t *Tx) ResolveReviewToReady(attemptID assignment.AttemptID, reason, actor string) error {
	affected, err := t.q.ResolveManualReviewToReady(t.ctx, sqlitedb.ResolveManualReviewToReadyParams{
		Resolution: "", ReviewedBy: actor, AttemptID: string(attemptID),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is not in manual review", ErrConflict, attemptID)
	}
	return t.RecordRepeatableEvent(attemptID, "operator_resolved",
		map[string]string{"decision": "retry", "actor": actor, "reason": reason})
}

// ResolveReviewToSettled is the operator deciding a held attempt may
// have run and must never run again. Audited the same way; the row's
// resolution keeps the vocabulary value, and the operator's words live
// in the event.
func (t *Tx) ResolveReviewToSettled(attemptID assignment.AttemptID, resolution, reason, actor string) error {
	affected, err := t.q.ResolveManualReviewToSettled(t.ctx, sqlitedb.ResolveManualReviewToSettledParams{
		Resolution: resolution, ReviewedBy: actor, AttemptID: string(attemptID),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is not in manual review", ErrConflict, attemptID)
	}
	return t.RecordRepeatableEvent(attemptID, "operator_resolved",
		map[string]string{"decision": "settle", "actor": actor, "reason": reason})
}

// Settle closes an attempt with an evidence-accurate resolution.
func (t *Tx) Settle(attemptID assignment.AttemptID, currentState AttemptState, resolution string) error {
	affected, err := t.q.SettleAttempt(t.ctx, sqlitedb.SettleAttemptParams{
		Resolution: resolution, AttemptID: string(attemptID), Current: string(currentState),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s is no longer %s", ErrConflict, attemptID, currentState)
	}
	return t.RecordEvent(attemptID, "attempt_settled", "attempt_settled")
}

// RequeueServing returns a serving's attempt to the queue, choosing
// between the two requeues by the state the caller read.
//
// The choice belongs here because the partition does. The two queries
// have identical bodies and differ only in which states they accept, and
// they are two rather than one on purpose: RequeueProvenInertAttempt
// admits `starting` and `running`, which are past the start
// authorization, and a caller may only reach it holding the daemon's
// proof that the start never took effect. Merging them into one guard
// would hand that permission to every caller, which is the at-most-once
// rule given away for a shorter file.
//
// What nothing here states is that their union covers the states a
// serving passes through. TestEveryServingStateCanBeDisposedOf does,
// by walking that set and requiring each of them to dispose.
func (t *Tx) RequeueServing(attempt Attempt) error {
	switch attempt.State {
	case AttemptStarting, AttemptRunning:
		return t.RequeueProvenInert(attempt.ID)
	default:
		return t.Requeue(attempt.ID)
	}
}

// HoldForReview parks an attempt for an operator with the reason the
// queue will show.
func (t *Tx) HoldForReview(attemptID assignment.AttemptID, reason string) error {
	affected, err := t.q.MarkAttemptManualReview(t.ctx, sqlitedb.MarkAttemptManualReviewParams{
		ReviewReason: reason, AttemptID: string(attemptID),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: attempt %s cannot enter review from its state", ErrConflict, attemptID)
	}
	return t.RecordRepeatableEvent(attemptID, "manual_review_requested",
		map[string]string{"reason": reason})
}

// RecordRepeatableEvent appends one lifecycle event whose kind can
// happen to an attempt more than once, sequencing the key so a second
// occurrence is a second event rather than a replay of the first.
//
// Use it for a decision a person makes: held for review, resolved,
// served again, held again. RecordEvent is for the rest, where the key
// already carries what makes the write distinct — a lease id, a delivery
// id — or where the thing can only happen once.
//
// It carries no conflict clause, unlike RecordEvent, because it has no
// reachable conflict to absorb — the query says why. A duplicate it
// cannot construct would be an error rather than a silent drop, which is
// the right answer for an append-only log.
func (t *Tx) RecordRepeatableEvent(attemptID assignment.AttemptID, kind string, detail map[string]string) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = t.q.InsertSequencedAttemptEvent(t.ctx, sqlitedb.InsertSequencedAttemptEventParams{
		AttemptID: string(attemptID), Kind: kind, DetailJson: string(encoded),
	})
	return err
}

// RecordEvent appends one lifecycle event, idempotently per key.
func (t *Tx) RecordEvent(attemptID assignment.AttemptID, idempotencyKey, kind string) error {
	_, err := t.q.InsertAttemptEvent(t.ctx, sqlitedb.InsertAttemptEventParams{
		AttemptID: string(attemptID), IdempotencyKey: idempotencyKey, Kind: kind, DetailJson: "{}",
	})
	return err
}

// RecordEventDetail appends one lifecycle event carrying structured
// detail. The detail is marshalled here so nothing unvalidated reaches
// the column, and it must never contain secrets — it is what an
// operator reads back.
func (t *Tx) RecordEventDetail(attemptID assignment.AttemptID, idempotencyKey, kind string, detail map[string]string) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = t.q.InsertAttemptEvent(t.ctx, sqlitedb.InsertAttemptEventParams{
		AttemptID: string(attemptID), IdempotencyKey: idempotencyKey, Kind: kind, DetailJson: string(encoded),
	})
	return err
}

// AckRequested marks a delivery's acknowledgement as in flight,
// immediately before the network call. It reports false when the
// delivery is already confirmed, so the caller skips the call.
func (t *Tx) AckRequested(deliveryID assignment.DeliveryID) (bool, error) {
	affected, err := t.q.MarkAckRequested(t.ctx, int64(deliveryID))
	return affected > 0, err
}

// AckConfirmed records an unambiguous acknowledgement. Already
// confirmed is idempotent success.
func (t *Tx) AckConfirmed(deliveryID assignment.DeliveryID) error {
	_, err := t.q.MarkAckConfirmed(t.ctx, int64(deliveryID))
	return err
}

// AckUncertain records an acknowledgement whose result is unknown — a
// timeout, an ambiguous error. The delivery stays retriable; the
// broker's redelivery plus idempotent recording converge it.
func (t *Tx) AckUncertain(deliveryID assignment.DeliveryID) error {
	_, err := t.q.MarkAckUncertain(t.ctx, int64(deliveryID))
	return err
}

// EnsureBinding records the neutral binding identity and returns its id.
func (t *Tx) EnsureBinding(targetID assignment.TargetID, providerKind string,
	sourceBindingKey assignment.SourceBindingKey) (assignment.BindingID, error) {
	b, err := t.q.InsertProviderBinding(t.ctx, sqlitedb.InsertProviderBindingParams{
		TargetID: string(targetID), ProviderKind: providerKind, SourceBindingKey: string(sourceBindingKey),
	})
	if err != nil {
		return 0, err
	}
	return assignment.BindingID(b.ID), nil
}

// ProviderContact is what one binding's loop last managed with its
// provider. Both facts are kept: a success clears the failure, and a
// failure leaves the last success alone, so a report can say how long a
// binding has been unable to reach anything.
type ProviderContact struct {
	BindingID   int64
	LastContact time.Time
	LastError   string
	LastErrorAt time.Time
}

// RecordProviderContact marks a provider call for this binding as having
// succeeded, and clears whatever was failing.
func (t *Tx) RecordProviderContact(bindingID assignment.BindingID, at time.Time) error {
	return t.q.RecordProviderContact(t.ctx, sqlitedb.RecordProviderContactParams{
		BindingID: int64(bindingID), At: at.UnixMilli(),
	})
}

// RecordProviderFailure records what this binding cannot currently do
// with its provider, without disturbing when it last could.
func (t *Tx) RecordProviderFailure(bindingID assignment.BindingID, at time.Time, reason string) error {
	return t.q.RecordProviderFailure(t.ctx, sqlitedb.RecordProviderFailureParams{
		BindingID: int64(bindingID), LastError: reason, At: at.UnixMilli(),
	})
}

// providerContactFromRow builds one binding's reach from its joined
// columns. Zero milliseconds is a moment that never happened — a binding
// that has never reached its provider and never failed either, which is
// the shape of one that has not run yet — and stays a zero time.
func providerContactFromRow(bindingID, contactMs int64, lastError string, errorMs int64) ProviderContact {
	c := ProviderContact{BindingID: bindingID, LastError: lastError}
	if contactMs > 0 {
		c.LastContact = time.UnixMilli(contactMs).UTC()
	}
	if errorMs > 0 {
		c.LastErrorAt = time.UnixMilli(errorMs).UTC()
	}
	return c
}

// bindingChildren are the tables that reference provider_bindings. A row
// in any of them outlives its binding only until the binding's own
// delete, which the foreign key then refuses, so every one of them has
// to go first.
//
// It is one declaration because two paths delete a binding and both need
// the same list: the startup pass that forgets unclaimed bindings, and
// uninstall. A table added later that names provider_bindings has to be
// added here, or excluded with a reason.
var bindingChildren = []string{"github_actions_binding_metadata", "provider_binding_contact"}

// ForgetUnclaimedBindings removes the binding rows configuration no
// longer claims, with the adapter metadata and reach records that hang
// off them, and reports how many went.
//
// A binding whose scale set was renamed or whose tier was removed is a
// row nothing serves: it holds no work, appears in every report, and
// there is no command that removes it. A binding that still owns a
// delivery is kept whatever configuration says — those rows are the
// trail of work that ran, and a report that lost the binding could not
// say whose work it was.
//
// The placeholder list is built here rather than through the generated
// layer: sqlc's sqlite engine has no slice parameter, and the ids come
// from the caller's own loop, never from input.
func (t *Tx) ForgetUnclaimedBindings(claimed []assignment.BindingID) (int, error) {
	if len(claimed) == 0 {
		// No claim at all is a caller with no bindings, which serve
		// refuses before reaching here. Deleting everything on an empty
		// list would turn that mistake into data loss.
		return 0, nil
	}
	held := placeholders(len(claimed))
	args := make([]any, len(claimed))
	for i, id := range claimed {
		args[i] = id
	}
	unclaimed := `
		SELECT id FROM provider_bindings
		WHERE id NOT IN (` + held + `)
		  AND NOT EXISTS (SELECT 1 FROM broker_deliveries d WHERE d.binding_id = provider_bindings.id)`

	// The dependents first: each references the binding row, so the
	// delete order is what the foreign keys allow.
	for _, table := range bindingChildren {
		if _, err := t.tx.Exec(
			`DELETE FROM `+table+` WHERE binding_id IN (`+unclaimed+`)`, args...); err != nil {
			return 0, err
		}
	}
	res, err := t.tx.Exec(`DELETE FROM provider_bindings WHERE id IN (`+unclaimed+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
