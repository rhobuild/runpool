package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store/sqlitedb"
)

var (
	ErrInvalidTransition = errors.New("invalid lease transition")
	// ErrStateConflict means the lease moved since the caller observed it;
	// the caller re-reads and re-decides rather than forcing the write.
	ErrStateConflict = errors.New("lease state conflict")
)

// LeaseAttempt claims a ready attempt and commits the lease that will
// serve it, in one transaction. The claim is the compare-and-swap that
// decides the winner: exactly one caller moves the attempt out of ready,
// and only that caller's lease row is written. Everyone else gets
// ErrConflict and re-reads.
//
// The claim runs first on purpose. Inserting the lease first would leave
// an orphan runtime row behind whenever the claim lost the race.
func (t *Tx) LeaseAttempt(attemptID assignment.AttemptID, bindingID assignment.BindingID, tierID assignment.TierID) (Lease, error) {
	affected, err := t.q.ClaimReadyAttempt(t.ctx, string(attemptID))
	if err != nil {
		return Lease{}, err
	}
	if affected == 0 {
		return Lease{}, fmt.Errorf("%w: attempt %s is not ready to lease", ErrConflict, attemptID)
	}
	id := newID(8)
	if err := t.q.InsertLease(t.ctx, sqlitedb.InsertLeaseParams{
		ID: id, BindingID: int64(bindingID), AttemptID: string(attemptID),
		TierID: string(tierID), State: string(LeaseReserved),
	}); err != nil {
		return Lease{}, err
	}
	if err := t.RecordEvent(attemptID, "lease_attached:"+id, EventLeaseAttached); err != nil {
		return Lease{}, err
	}
	return t.LeaseByID(assignment.LeaseID(id))
}

// TransitionLease moves a lease along the state machine. The write is
// guarded by the expected current state: if the lease moved meanwhile,
// nothing changes and ErrStateConflict reports where it actually is.
func (t *Tx) TransitionLease(id assignment.LeaseID, from, to LeaseState) error {
	if !ValidTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	affected, err := t.q.TransitionLease(t.ctx, sqlitedb.TransitionLeaseParams{
		Next: string(to), ID: string(id), Current: string(from),
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		current, err := t.LeaseByID(id)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: lease %s is %s, expected %s", ErrStateConflict, id, current.State, from)
	}
	return nil
}

// SetLeaseRuntimeName records the name the runtime registered under. It
// is the correlation handle for provider lifecycle events: each lease
// registers its own, so the name cannot cross attempts.
func (t *Tx) SetLeaseRuntimeName(id assignment.LeaseID, runtimeName assignment.RuntimeName) error {
	affected, err := t.q.SetLeaseRuntimeName(t.ctx, sqlitedb.SetLeaseRuntimeNameParams{
		RuntimeName: requiredText(runtimeName), ID: string(id),
	})
	return mustAffect(affected, err)
}

// RecordLeaseStartObservation keeps what this serving measured about
// whether the workload began.
//
// It is written on the way into the cleanup that ends a serving, after
// every refinement that can overrule the measurement and before the
// first destructive step -- so a retry of that cleanup reads back the
// answer the pass that failed had already reached. A later measurement
// overwrites an earlier one rather than being refused, because a later
// establishing measurement is a better one: the capsule's own account of
// itself arrives after the daemon's, and the provider's after that.
//
// Only an observation that establishes something belongs here: the
// column admits those four and refuses the rest, which is deliberate and
// stays that way. A guard in this method would swallow the refusal --
// and TestTheColumnRefusesAnEmptyMeasurement exists to say the table,
// not a removable Go check, is what keeps the two spellings of "nothing"
// from both being storable. The lease manager filters before calling so
// that nothing is asked for; the column is what makes that true.
func (t *Tx) RecordLeaseStartObservation(id assignment.LeaseID, obs assignment.ExecutionObservation) error {
	affected, err := t.q.SetLeaseStartObservation(t.ctx, sqlitedb.SetLeaseStartObservationParams{
		StartObservation: requiredText(obs), ID: string(id),
	})
	return mustAffect(affected, err)
}

func (t *Tx) LeaseByID(id assignment.LeaseID) (Lease, error) {
	r, err := t.q.GetLease(t.ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrNotFound, id)
	}
	return leaseFromRow(r), err
}

// AttemptOfRuntimeName answers which attempt a named runtime ran for.
//
// Attribution is a historical question. A report of a run that has
// already finished is the case it exists for, and by then that run's
// lease is released - so unlike everything else that reads a lease, this
// does not exclude terminal ones. The name is minted per lease, so it
// resolves to one attempt or to none.
func (t *Tx) AttemptOfRuntimeName(runtimeName assignment.RuntimeName) (assignment.AttemptID, error) {
	if runtimeName == "" {
		return "", fmt.Errorf("%w: no runtime name to resolve", ErrNotFound)
	}
	attemptID, err := t.q.GetAttemptIDByRuntimeName(t.ctx, requiredText(runtimeName))
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no lease owns runtime %s", ErrNotFound, runtimeName)
	}
	return assignment.AttemptID(attemptID), err
}

// LeaseByAttempt finds the newest lease of an attempt. An attempt holds
// one lease per serving and at most one of them is live, so the newest is
// the live one while there is one, and the last serving's otherwise —
// which is what a caller repairing a crash window is asking about.
//
// Ordered by rowid, not created_at: two servings of one attempt can share
// a second, and a tie broken by a random id would answer this with either
// of them. Insertion order is what "newest" means here and rowid is what
// records it.
func (t *Tx) LeaseByAttempt(attemptID assignment.AttemptID) (Lease, error) {
	r, err := t.q.GetNewestLeaseByAttempt(t.ctx, string(attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, fmt.Errorf("%w: no lease serves attempt %s", ErrNotFound, attemptID)
	}
	return leaseFromRow(r), err
}

// RecentReleasedLeases returns the newest released leases, oldest first,
// and the total number of released leases there are.
//
// Reporting wants recent history, not all of it. A snapshot that
// enumerated every state grew with every job the host had ever run, and
// each lease in one costs two more queries. The count comes back
// alongside because the two places that show it — `status`, and the line
// `uninstall` prints before destroying the books — are counts, and a
// count must stay true even when the rows behind it are not all loaded.
//
// Recency is judged by `updated_at`, which for a released lease is when
// it finished: nothing writes that column after the transition into
// released. Ranking by `created_at` would rank by when the job started,
// so a lease that wedged for weeks and was resolved a minute ago sorts
// below everything begun since — and the record an operator is looking
// for is the one the bound hides.
//
// Selection is newest-first because that is what "recent" means; the
// result is reversed before returning because every consumer of a lease
// list here reads oldest-first, and both timestamps are published per
// lease, so the order is observable from outside.
func (t *Tx) RecentReleasedLeases(limit int) (leases []Lease, total int, err error) {
	count, err := t.q.CountReleasedLeases(t.ctx)
	if err != nil {
		return nil, 0, err
	}
	total = int(count)
	if limit <= 0 {
		return nil, total, nil
	}
	rows, err := t.q.ListRecentReleasedLeases(t.ctx, int64(limit))
	if err != nil {
		return nil, 0, err
	}
	leases = make([]Lease, len(rows))
	for i, r := range rows {
		leases[i] = leaseFromRow(r)
	}
	slices.Reverse(leases)
	return leases, total, nil
}

// CountPrunableLeases reports how many released lease records a prune would
// remove now, for a dry run.
//
// Age is measured from `updated_at`, the last transition — which for a
// released lease is when it finished, because nothing writes that column
// afterwards. The window is how long a finished record is kept, so
// measuring from `created_at` would forget by how long ago the job
// started: a lease that sat in quarantine for months and was resolved a
// minute ago starts outside every window and would be deleted within a
// reconcile tick of finishing.
//
// Two guards decide the set, and each one prevents a different loss.
//
// The attempt must be resolved. StrandedAttempts finds attempts left open
// by a crash *by joining through their released lease* — so deleting that
// lease makes the attempt unreachable by every working set there is,
// permanently. It is the same guard PurgeLease states, spelled from the
// same list.
//
// And the lease must own no resource intent. resource_intents references
// capsule_leases without ON DELETE CASCADE and foreign keys are enforced,
// so a lease whose cleanup never finished would fail the delete rather
// than be skipped. Skipping is what is wanted: a surviving intent is a
// real leak, and it should stay visible instead of losing its row.
func (t *Tx) CountPrunableLeases(before time.Time, limit int) (int, error) {
	n, err := t.q.CountPrunableReleasedLeases(t.ctx, sqlitedb.CountPrunableReleasedLeasesParams{
		Before: before.Unix(), RowLimit: int64(limit),
	})
	return int(n), err
}

// PruneLeaseHistory deletes the record of finished leases older than
// before, oldest first, at most limit of them. It returns how many went.
//
// Only capsule_leases. The attempt is the record of what the work did and
// outlives its runtime plumbing — the schema was built for exactly this,
// which is why no attempt row names a lease.
func (t *Tx) PruneLeaseHistory(before time.Time, limit int) (int, error) {
	n, err := t.q.PruneReleasedLeases(t.ctx, sqlitedb.PruneReleasedLeasesParams{
		Before: before.Unix(), RowLimit: int64(limit),
	})
	return int(n), err
}

// LeasesInStates lists leases in any of the given states, oldest first —
// the reconciler's working set.
func (t *Tx) LeasesInStates(states ...LeaseState) ([]Lease, error) {
	if len(states) == 0 {
		return nil, nil
	}
	held := placeholders(len(states))
	args := make([]any, len(states))
	for i, s := range states {
		args[i] = s
	}
	rows, err := t.tx.Query(
		selectLease+`WHERE state IN (`+held+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		l, err := t.scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PurgeLease deletes one lease row once its resources are gone. Nothing
// calls it today: uninstall clears the whole machine with PurgeEverything
// and cleanup removes Docker objects without touching lease rows, because
// a released lease is useful history. It is kept for the guard it states,
// which the generated retention queries honour as well.
//
// It refuses while the attempt it serves is still unresolved: deleting
// the runtime record of live work leaves an attempt nothing can finish
// or explain. Resolve the attempt first — settle it, or hold it for
// review — and the lease becomes purgeable.
func (t *Tx) PurgeLease(id assignment.LeaseID) error {
	affected, err := t.q.PurgeResolvedLease(t.ctx, string(id))
	if err != nil {
		return err
	}
	if affected == 0 {
		if _, err := t.LeaseByID(assignment.LeaseID(id)); err != nil {
			return err
		}
		return fmt.Errorf("%w: lease %s still serves an unresolved attempt", ErrConflict, id)
	}
	return nil
}

const selectLease = `SELECT id, binding_id, attempt_id, tier_id, state, runtime_name, start_observation, created_at, updated_at FROM capsule_leases `

type rowScanner interface{ Scan(...any) error }

func (t *Tx) scanLease(r rowScanner) (Lease, error) {
	var l Lease
	var created, updated int64
	// NULL is an absence: a serving that recorded nothing, a runtime that
	// never registered. NullString renders both as the empty string, which
	// is each type's own zero -- NoObservation by name for the measurement,
	// and for the runtime a name no lookup will answer for. The storage
	// represents the absence and the domain's zero value means it; this is
	// the one line where they meet.
	var observed, runtime sql.NullString
	err := r.Scan(&l.ID, &l.BindingID, &l.AttemptID, &l.TierID, &l.State,
		&runtime, &observed, &created, &updated)
	l.RuntimeName = assignment.RuntimeName(runtime.String)
	l.StartObservation = assignment.ExecutionObservation(observed.String)
	l.CreatedAt = time.Unix(created, 0).UTC()
	l.UpdatedAt = time.Unix(updated, 0).UTC()
	return l, err
}

func leaseFromRow(r sqlitedb.CapsuleLease) Lease {
	return Lease{
		ID:               assignment.LeaseID(r.ID),
		BindingID:        assignment.BindingID(r.BindingID),
		AttemptID:        assignment.AttemptID(r.AttemptID),
		TierID:           assignment.TierID(r.TierID),
		State:            LeaseState(r.State),
		RuntimeName:      assignment.RuntimeName(r.RuntimeName.String),
		StartObservation: assignment.ExecutionObservation(r.StartObservation.String),
		CreatedAt:        unixTime(r.CreatedAt),
		UpdatedAt:        unixTime(r.UpdatedAt),
	}
}

const selectIntent = `SELECT id, lease_id, kind, role, name, docker_id, state, retries, last_error, not_before, created_at FROM resource_intents `

func (t *Tx) scanIntent(r rowScanner) (ResourceIntent, error) {
	var in ResourceIntent
	var kind, role, state string
	var notBefore, created int64
	err := r.Scan(&in.ID, &in.LeaseID, &kind, &role, &in.Name, &in.DockerID,
		&state, &in.Retries, &in.LastError, &notBefore, &created)
	in.Kind = ResourceKind(kind)
	in.Role = ResourceRole(role)
	in.State = ResourceState(state)
	in.NotBefore = optionalUnixTime(notBefore)
	in.CreatedAt = unixTime(created)
	return in, err
}

func resourceFromRow(r sqlitedb.ResourceIntent) ResourceIntent {
	return ResourceIntent{
		ID:        assignment.ResourceIntentID(r.ID),
		LeaseID:   assignment.LeaseID(r.LeaseID),
		Kind:      ResourceKind(r.Kind),
		Role:      ResourceRole(r.Role),
		Name:      r.Name,
		DockerID:  r.DockerID,
		State:     ResourceState(r.State),
		Retries:   r.Retries,
		LastError: r.LastError,
		NotBefore: optionalUnixTime(r.NotBefore),
		CreatedAt: unixTime(r.CreatedAt),
	}
}

func resourcesFromRows(rows []sqlitedb.ResourceIntent) []ResourceIntent {
	out := make([]ResourceIntent, len(rows))
	for i, r := range rows {
		out[i] = resourceFromRow(r)
	}
	return out
}

// PlanResource commits the intention to create one object, before any
// external effect. The deterministic name is the recovery handle: a
// crash after the create call finds the object — or proves its absence
// — by the name this row already carries.
func (t *Tx) PlanResource(leaseID assignment.LeaseID, kind ResourceKind, role ResourceRole, name string) (assignment.ResourceIntentID, error) {
	id, err := t.q.InsertResourceIntent(t.ctx, sqlitedb.InsertResourceIntentParams{
		LeaseID: string(leaseID), Kind: string(kind), Role: string(role), Name: name,
	})
	return assignment.ResourceIntentID(id), err
}

// MarkResourceCreating records that the create call is about to run: the
// window in which existence is ambiguous, resolved by the name.
func (t *Tx) MarkResourceCreating(id assignment.ResourceIntentID) error {
	affected, err := t.q.MarkResourceCreating(t.ctx, sqlitedb.MarkResourceCreatingParams{
		Creating: string(ResourceCreating), ID: int64(id), Planned: string(ResourcePlanned),
	})
	return mustAffect(affected, err)
}

// MarkResourcePresent confirms the object exists under this id.
func (t *Tx) MarkResourcePresent(id assignment.ResourceIntentID, dockerID string) error {
	affected, err := t.q.MarkResourcePresent(t.ctx, sqlitedb.MarkResourcePresentParams{
		Present: string(ResourcePresent), DockerID: dockerID, ID: int64(id),
		Planned: string(ResourcePlanned), Creating: string(ResourceCreating),
	})
	return mustAffect(affected, err)
}

// MarkResourceCleanup queues every one of a lease's intents for
// removal. Objects that never confirmed are queued too: a creating
// intent's object may exist, and only the delete path proves otherwise.
func (t *Tx) MarkResourceCleanup(leaseID assignment.LeaseID) error {
	return t.q.MarkResourcesForCleanup(t.ctx, sqlitedb.MarkResourcesForCleanupParams{
		CleanupPending: string(ResourceCleanupPending), LeaseID: string(leaseID),
		Planned: string(ResourcePlanned), Creating: string(ResourceCreating),
		Present: string(ResourcePresent),
	})
}

// MarkResourceDeleting records that the delete call is about to run.
func (t *Tx) MarkResourceDeleting(id assignment.ResourceIntentID) error {
	affected, err := t.q.MarkResourceDeleting(t.ctx, sqlitedb.MarkResourceDeletingParams{
		Deleting: string(ResourceDeleting), ID: int64(id),
		CleanupPending: string(ResourceCleanupPending),
	})
	return mustAffect(affected, err)
}

// ForgetResource is the intent reaching absent: the object is proven
// gone, so the row goes with it.
func (t *Tx) ForgetResource(id assignment.ResourceIntentID) error {
	affected, err := t.q.DeleteResourceIntent(t.ctx, int64(id))
	return mustAffect(affected, err)
}

// RecordResourceError books a failed attempt against the intent:
// bounded retries with exponential backoff and jitter live on the row,
// so the periodic reconciler paces each resource instead of hammering
// the daemon.
func (t *Tx) RecordResourceError(id assignment.ResourceIntentID, attemptErr error, notBefore time.Time) error {
	msg := ""
	if attemptErr != nil {
		msg = attemptErr.Error()
	}
	affected, err := t.q.RecordResourceError(t.ctx, sqlitedb.RecordResourceErrorParams{
		LastError: msg, NotBefore: notBefore.Unix(), ID: int64(id),
	})
	return mustAffect(affected, err)
}

// Resources lists a lease's intents, oldest first.
func (t *Tx) Resources(leaseID assignment.LeaseID) ([]ResourceIntent, error) {
	rows, err := t.q.ListResourcesByLease(t.ctx, string(leaseID))
	if err != nil {
		return nil, err
	}
	return resourcesFromRows(rows), nil
}

// pendingRemovals lists the intents whose objects must go and are due,
// bounded and backoff-aware. The periodic reconciler does not use it — it
// works from live leases and asks IntentsDue per lease — so this is the
// query that states the backoff ordering, and its test is where that
// ordering is pinned.
func (t *Tx) pendingRemovals(now time.Time, limit int) ([]ResourceIntent, error) {
	rows, err := t.q.ListPendingResourceRemovals(t.ctx, sqlitedb.ListPendingResourceRemovalsParams{
		CleanupPending: string(ResourceCleanupPending), Deleting: string(ResourceDeleting),
		Now: now.Unix(), RowLimit: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	return resourcesFromRows(rows), nil
}
