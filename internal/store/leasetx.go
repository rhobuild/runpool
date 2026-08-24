package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
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
func (t *Tx) LeaseAttempt(attemptID assignment.AttemptID, bindingID assignment.BindingID, tierID string) (Lease, error) {
	affected, err := t.q.ClaimReadyAttempt(t.ctx, string(attemptID))
	if err != nil {
		return Lease{}, err
	}
	if affected == 0 {
		return Lease{}, fmt.Errorf("%w: attempt %s is not ready to lease", ErrConflict, attemptID)
	}
	id := newID(8)
	if _, err := t.tx.Exec(
		`INSERT INTO capsule_leases (id, binding_id, attempt_id, tier_id, state) VALUES (?, ?, ?, ?, ?)`,
		id, bindingID, attemptID, tierID, LeaseReserved); err != nil {
		return Lease{}, err
	}
	if err := t.RecordEvent(attemptID, "lease_attached:"+id, "lease_attached"); err != nil {
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
	res, err := t.tx.Exec(
		`UPDATE capsule_leases SET state = ?, updated_at = unixepoch() WHERE id = ? AND state = ?`,
		to, id, from)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
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
	return t.mustAffect(t.tx.Exec(
		`UPDATE capsule_leases SET runtime_name = ?, updated_at = unixepoch() WHERE id = ?`,
		runtimeName, id))
}

// RecordLeaseStartObservation keeps what this serving measured about
// whether the workload began.
//
// It is written on the way into the cleanup that ends a serving, after
// every refinement that can overrule the measurement and before the
// first destructive step -- so a retry of that cleanup reads back the
// answer the pass that failed had already reached. The write is
// unconditional because a later establishing measurement is a better one:
// the capsule's own account of itself arrives after the daemon's, and the
// provider's after that.
func (t *Tx) RecordLeaseStartObservation(id assignment.LeaseID, obs assignment.ExecutionObservation) error {
	return t.mustAffect(t.tx.Exec(
		`UPDATE capsule_leases SET start_observation = ?, updated_at = unixepoch() WHERE id = ?`,
		obs, id))
}

func (t *Tx) LeaseByID(id assignment.LeaseID) (Lease, error) {
	l, err := t.scanLease(t.tx.QueryRow(selectLease+`WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, fmt.Errorf("%w: lease %s", ErrNotFound, id)
	}
	return l, err
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
	var attemptID assignment.AttemptID
	err := t.tx.QueryRow(
		`SELECT attempt_id FROM capsule_leases WHERE runtime_name = ? ORDER BY rowid DESC LIMIT 1`,
		runtimeName).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no lease owns runtime %s", ErrNotFound, runtimeName)
	}
	return attemptID, err
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
	l, err := t.scanLease(t.tx.QueryRow(
		selectLease+`WHERE attempt_id = ? ORDER BY rowid DESC LIMIT 1`, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, fmt.Errorf("%w: no lease serves attempt %s", ErrNotFound, attemptID)
	}
	return l, err
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
	if err := t.tx.QueryRow(
		`SELECT count(*) FROM capsule_leases WHERE state = ?`, LeaseReleased).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		return nil, total, nil
	}
	rows, err := t.tx.Query(
		selectLease+`WHERE state = ? ORDER BY updated_at DESC, id DESC LIMIT ?`, LeaseReleased, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		l, err := t.scanLease(rows)
		if err != nil {
			return nil, 0, err
		}
		leases = append(leases, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	slices.Reverse(leases)
	return leases, total, nil
}

// prunableReleasedLeases selects released leases old enough to forget.
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
const prunableReleasedLeases = `
	SELECT id FROM capsule_leases
	WHERE state = 'released' AND updated_at < ?
	  AND attempt_id NOT IN (
	      SELECT id FROM assignment_attempts
	      WHERE state IN (` + openAttemptStates + `))
	  AND id NOT IN (SELECT lease_id FROM resource_intents)
	ORDER BY updated_at LIMIT ?`

// openAttemptStates is the SQL list of attempt states that are not
// resolved: an attempt in one of them is still serving live work.
//
// It is spelled once because the two deletes that consult it are the
// only things standing between a running job and a lease row that is its
// sole handle — StrandedAttempts reaches an open attempt by joining
// through its lease, so a delete working from a shorter list makes that
// attempt unreachable by every working set, permanently. The schema
// keeps its own copy in the one_open_attempt_per_workload index, which
// is the authority the two agree with.
const openAttemptStates = `'ready', 'leased', 'preparing', 'prepared',
	                       'starting', 'running', 'manual_review'`

// CountPrunableLeases reports how many lease records a prune would remove
// now, for a dry run.
func (t *Tx) CountPrunableLeases(before time.Time, limit int) (int, error) {
	var n int
	err := t.tx.QueryRow(
		`SELECT count(*) FROM (`+prunableReleasedLeases+`)`, before.Unix(), limit).Scan(&n)
	return n, err
}

// PruneLeaseHistory deletes the record of finished leases older than
// before, oldest first, at most limit of them. It returns how many went.
//
// Only capsule_leases. The attempt is the record of what the work did and
// outlives its runtime plumbing — the schema was built for exactly this,
// which is why no attempt row names a lease.
func (t *Tx) PruneLeaseHistory(before time.Time, limit int) (int, error) {
	res, err := t.tx.Exec(
		`DELETE FROM capsule_leases WHERE id IN (`+prunableReleasedLeases+`)`, before.Unix(), limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
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
// which retention honours in prunableReleasedLeases.
//
// It refuses while the attempt it serves is still unresolved: deleting
// the runtime record of live work leaves an attempt nothing can finish
// or explain. Resolve the attempt first — settle it, or hold it for
// review — and the lease becomes purgeable.
func (t *Tx) PurgeLease(id assignment.LeaseID) error {
	res, err := t.tx.Exec(`
		DELETE FROM capsule_leases
		WHERE id = ? AND attempt_id NOT IN (
			SELECT id FROM assignment_attempts
			WHERE state IN (`+openAttemptStates+`))`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
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
	err := r.Scan(&l.ID, &l.BindingID, &l.AttemptID, &l.TierID, &l.State,
		&l.RuntimeName, &l.StartObservation, &created, &updated)
	l.CreatedAt = time.Unix(created, 0).UTC()
	l.UpdatedAt = time.Unix(updated, 0).UTC()
	return l, err
}

const selectIntent = `SELECT id, lease_id, kind, role, name, docker_id, state, retries, last_error, not_before, created_at FROM resource_intents `

func (t *Tx) scanIntent(r rowScanner) (ResourceIntent, error) {
	var in ResourceIntent
	var created int64
	err := r.Scan(&in.ID, &in.LeaseID, &in.Kind, &in.Role, &in.Name, &in.DockerID,
		&in.State, &in.Retries, &in.LastError, &in.NotBefore, &created)
	in.CreatedAt = time.Unix(created, 0).UTC()
	return in, err
}

// PlanResource commits the intention to create one object, before any
// external effect. The deterministic name is the recovery handle: a
// crash after the create call finds the object — or proves its absence
// — by the name this row already carries.
func (t *Tx) PlanResource(leaseID assignment.LeaseID, kind ResourceKind, role, name string) (assignment.ResourceIntentID, error) {
	res, err := t.tx.Exec(
		`INSERT INTO resource_intents (lease_id, kind, role, name) VALUES (?, ?, ?, ?)`,
		leaseID, kind, role, name)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return assignment.ResourceIntentID(id), err
}

// MarkResourceCreating records that the create call is about to run: the
// window in which existence is ambiguous, resolved by the name.
func (t *Tx) MarkResourceCreating(id assignment.ResourceIntentID) error {
	return t.mustAffect(t.tx.Exec(
		`UPDATE resource_intents SET state = 'creating', updated_at = unixepoch()
		 WHERE id = ? AND state = 'planned'`, id))
}

// MarkResourcePresent confirms the object exists under this id.
func (t *Tx) MarkResourcePresent(id assignment.ResourceIntentID, dockerID string) error {
	return t.mustAffect(t.tx.Exec(
		`UPDATE resource_intents SET state = 'present', docker_id = ?, updated_at = unixepoch()
		 WHERE id = ? AND state IN ('planned', 'creating')`, dockerID, id))
}

// MarkResourceCleanup queues every one of a lease's intents for
// removal. Objects that never confirmed are queued too: a creating
// intent's object may exist, and only the delete path proves otherwise.
func (t *Tx) MarkResourceCleanup(leaseID assignment.LeaseID) error {
	_, err := t.tx.Exec(
		`UPDATE resource_intents SET state = 'cleanup_pending', updated_at = unixepoch()
		 WHERE lease_id = ? AND state IN ('planned', 'creating', 'present')`, leaseID)
	return err
}

// MarkResourceDeleting records that the delete call is about to run.
func (t *Tx) MarkResourceDeleting(id assignment.ResourceIntentID) error {
	return t.mustAffect(t.tx.Exec(
		`UPDATE resource_intents SET state = 'deleting', updated_at = unixepoch()
		 WHERE id = ? AND state IN ('cleanup_pending', 'deleting')`, id))
}

// ForgetResource is the intent reaching absent: the object is proven
// gone, so the row goes with it.
func (t *Tx) ForgetResource(id assignment.ResourceIntentID) error {
	return t.mustAffect(t.tx.Exec(`DELETE FROM resource_intents WHERE id = ?`, id))
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
	return t.mustAffect(t.tx.Exec(
		`UPDATE resource_intents SET retries = retries + 1, last_error = ?, not_before = ?, updated_at = unixepoch()
		 WHERE id = ?`, msg, notBefore.Unix(), id))
}

// Resources lists a lease's intents, oldest first.
func (t *Tx) Resources(leaseID assignment.LeaseID) ([]ResourceIntent, error) {
	rows, err := t.tx.Query(selectIntent+`WHERE lease_id = ? ORDER BY id`, leaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResourceIntent
	for rows.Next() {
		in, err := t.scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// pendingRemovals lists the intents whose objects must go and are due,
// bounded and backoff-aware. The periodic reconciler does not use it — it
// works from live leases and asks IntentsDue per lease — so this is the
// query that states the backoff ordering, and its test is where that
// ordering is pinned.
func (t *Tx) pendingRemovals(now time.Time, limit int) ([]ResourceIntent, error) {
	rows, err := t.tx.Query(selectIntent+`
		WHERE state IN ('cleanup_pending', 'deleting') AND not_before <= ?
		ORDER BY not_before, id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResourceIntent
	for rows.Next() {
		in, err := t.scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
