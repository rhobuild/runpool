package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Snapshot is everything an operator needs to answer "what does this
// instance own and why is it in this shape" without opening SQLite.
type Snapshot struct {
	InstanceID    string
	SchemaVersion int
	Bindings      []BindingInfo
	// Leases is every live lease plus the most recent released ones —
	// not the whole history. A live lease is never omitted: a report that
	// lost one would call its containers orphans, and cleanup would delete
	// the resources of a job that is still running.
	Leases []Lease
	// ReleasedTotal is how many released leases exist, including those
	// Leases does not carry. The two places that show a number — status,
	// and the line uninstall prints before destroying the books — need it
	// to stay true once the rows behind it are bounded.
	ReleasedTotal int
	Attempts      map[assignment.LeaseID]Attempt
	Resources     map[assignment.LeaseID][]ResourceIntent
	// Queued is how many attempts wait for admission, per binding id. It
	// is counted rather than listed because the rest of this snapshot is
	// keyed by lease, and an attempt waiting for admission has none — so
	// without this a queue that stopped draining is invisible until an
	// operator thinks to look for something the report never mentions.
	Queued     map[int64]int
	CacheLanes []CacheLaneInfo
	// Pressure is the disk monitor's last persisted verdict; nil until
	// the monitor has run once.
	Pressure *PressureInfo
	// Sandbox is the egress sandbox's last rediscovery pass; nil until
	// one has completed, and nil for the whole life of an instance that
	// maintains no policy at all.
	Sandbox *SandboxPass
}

// BindingInfo reports one configured source of work in neutral terms.
// SourceBindingKey is the provider's own identity, versioned and opaque
// here; for a GitHub Actions binding it reads as scope, URL, runner
// group and scale set name, which is enough for an operator to tell two
// bindings apart without the adapter's table being consulted.
type BindingInfo struct {
	ID               assignment.BindingID
	TargetID         assignment.TargetID
	ProviderKind     string
	SourceBindingKey assignment.SourceBindingKey
	// Contact is what this binding's loop last managed with its provider.
	// A binding that has never run carries the zero value, which is not
	// the same as one that is failing: the first has nothing to report,
	// the second has a reason.
	Contact ProviderContact
}

// Bindings lists every recorded binding with its provider reach, in a
// stable order and in one query: a list plus a lookup joined in Go is
// two reads a write can straddle.
func (t *Tx) Bindings() ([]BindingInfo, error) {
	rows, err := t.q.ListProviderBindingsWithContact(t.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BindingInfo, len(rows))
	for i, r := range rows {
		out[i] = BindingInfo{
			ID: assignment.BindingID(r.ID), TargetID: assignment.TargetID(r.TargetID), ProviderKind: r.ProviderKind,
			SourceBindingKey: assignment.SourceBindingKey(r.SourceBindingKey),
			Contact:          providerContactFromRow(r.ID, r.LastContactAtMs, r.LastError, r.LastErrorAtMs),
		}
	}
	return out, nil
}

// CacheLaneInfo reports a lane and who holds it; an empty LeasedBy is a
// free lane whose warm data waits for the next job. LastUsed is the
// lane's LRU clock: it advances when a lease takes the lane, and GC
// evicts free lanes oldest-first by it.
type CacheLaneInfo struct {
	ID               string
	SourceProjectKey string
	Generation       string
	LeasedBy         assignment.LeaseID
	LastUsed         int64
}

// Snapshot collects the instance's whole durable picture in one
// transaction, so what it reports is internally consistent.
func (s *Store) Snapshot() (Snapshot, error) {
	snap := Snapshot{
		InstanceID: s.instanceID,
		Attempts:   map[assignment.LeaseID]Attempt{},
		Resources:  map[assignment.LeaseID][]ResourceIntent{},
		Queued:     map[int64]int{},
	}
	version, err := s.SchemaVersion()
	if err != nil {
		return snap, err
	}
	snap.SchemaVersion = version

	err = s.Tx(context.Background(), func(tx *Tx) error {
		if snap.Bindings, err = tx.Bindings(); err != nil {
			return err
		}
		// Live leases whole, released ones bounded. Asking for every
		// state made the snapshot grow with each job the host ever ran,
		// and the per-lease reads below turned that into two queries per
		// row — so `runpool status` cost rose with history rather than
		// with what the instance is doing.
		if snap.Leases, err = tx.LeasesInStates(LiveLeaseStates...); err != nil {
			return err
		}
		recent, total, err := tx.RecentReleasedLeases(ReportedReleasedLeases)
		if err != nil {
			return err
		}
		snap.Leases = append(snap.Leases, recent...)
		snap.ReleasedTotal = total
		// Two set reads rather than two per lease: this runs on the one
		// connection every lease transition also needs.
		if snap.Attempts, err = tx.attemptsOfLeases(snap.Leases); err != nil {
			return err
		}
		if snap.Resources, err = tx.resourcesOfLeases(snap.Leases); err != nil {
			return err
		}
		for _, b := range snap.Bindings {
			queued, err := tx.CountReadyAttempts(b.ID)
			if err != nil {
				return err
			}
			if queued > 0 {
				snap.Queued[int64(b.ID)] = int(queued)
			}
		}
		if snap.CacheLanes, err = tx.CacheLanes(); err != nil {
			return err
		}
		snap.Pressure, err = tx.Pressure()
		if err != nil {
			return err
		}
		snap.Sandbox, err = tx.SandboxPass()
		return err
	})
	return snap, err
}

// CacheLanes lists every lane. GC plans over it (free lanes, LRU by
// LastUsed) and the snapshot reports it; both need the same rows.
func (t *Tx) CacheLanes() ([]CacheLaneInfo, error) {
	rows, err := t.tx.Query(`
		SELECT l.id, p.source_project_key, l.generation, coalesce(l.leased_by, ''), l.last_used
		FROM cache_lanes l JOIN cache_projects p ON p.id = l.project_id
		ORDER BY p.source_project_key, l.generation, l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheLaneInfo
	for rows.Next() {
		var c CacheLaneInfo
		if err := rows.Scan(&c.ID, &c.SourceProjectKey, &c.Generation, &c.LeasedBy, &c.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// selectAttempt is the attempt column list this file's set read scans.
// It repeats the generated GetAttempt query's projection by hand,
// because sqlc's sqlite engine has no slice parameter and a set read
// cannot be generated.
//
// Two column lists that must agree and cannot be made to share one:
// TestTheSetReadAgreesWithTheGeneratedQuery reads the same attempt both
// ways and requires the results to be identical. That catches a column
// added to one and not the other, and a reordering that swaps two
// columns of the same type, which no compiler can.
const selectAttempt = `SELECT id, delivery_id, binding_id, source_workload_key, tenant_key,
       project_key, state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at FROM assignment_attempts `

// attemptsOfLeases reads the attempt each lease serves in one query.
//
// One read per lease made `runpool status` cost two round trips per row
// on the single connection every lease transition also waits for, and a
// snapshot carries up to a hundred leases.
func (t *Tx) attemptsOfLeases(leases []Lease) (map[assignment.LeaseID]Attempt, error) {
	out := make(map[assignment.LeaseID]Attempt, len(leases))
	if len(leases) == 0 {
		return out, nil
	}
	// Distinct attempt ids. Several leases can name one attempt: the
	// index that forbids two live leases per attempt says nothing about
	// a released one, so an attempt returned to the queue and served
	// again is named by both.
	args := make([]any, 0, len(leases))
	asked := make(map[assignment.AttemptID]struct{}, len(leases))
	for _, l := range leases {
		if _, dup := asked[l.AttemptID]; dup {
			continue
		}
		asked[l.AttemptID] = struct{}{}
		args = append(args, l.AttemptID)
	}
	rows, err := t.tx.Query(selectAttempt+
		`WHERE id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byAttempt := make(map[assignment.AttemptID]Attempt, len(args))
	for rows.Next() {
		var r sqlitedb.AssignmentAttempt
		if err := rows.Scan(&r.ID, &r.DeliveryID, &r.BindingID, &r.SourceWorkloadKey,
			&r.TenantKey, &r.ProjectKey, &r.State, &r.ExecutionEvidence, &r.Resolution,
			&r.ReviewReason, &r.ReviewedAt, &r.ReviewedBy, &r.ReceivedAt, &r.SettledAt); err != nil {
			return nil, err
		}
		byAttempt[assignment.AttemptID(r.ID)] = fromRow(r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Keyed by lease, because "what is this lease serving" is the
	// question the report asks. Keying by attempt made two leases of one
	// attempt collapse into a single entry, and released leases are
	// appended after live ones, so the row that lost its attempt was
	// always the lease still running.
	for _, l := range leases {
		attempt, ok := byAttempt[l.AttemptID]
		if !ok {
			return nil, fmt.Errorf("lease %s names attempt %s, which does not exist", l.ID, l.AttemptID)
		}
		out[l.ID] = attempt
	}
	return out, nil
}

// resourcesOfLeases reads every lease's resource intents in one query,
// for the same reason attemptsOfLeases exists.
func (t *Tx) resourcesOfLeases(leases []Lease) (map[assignment.LeaseID][]ResourceIntent, error) {
	out := make(map[assignment.LeaseID][]ResourceIntent, len(leases))
	if len(leases) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(leases))
	for _, l := range leases {
		args = append(args, l.ID)
	}
	rows, err := t.tx.Query(selectIntent+
		`WHERE lease_id IN (`+placeholders(len(args))+`) ORDER BY lease_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		in, err := t.scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out[in.LeaseID] = append(out[in.LeaseID], in)
	}
	return out, rows.Err()
}

// placeholders builds the `?,?,?` list a set read needs. sqlc's sqlite
// engine has no slice parameter, and the values are always ids the
// caller already holds, never input.
//
// One copy, because three grew: two spelled it inline, and both named
// their local after this function — in files that never called it, so
// nothing broke and nothing said anything.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
