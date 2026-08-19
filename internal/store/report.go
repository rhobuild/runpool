package store

import (
	"context"
	"strings"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"
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
	Attempts      map[string]Attempt          // by lease id
	Resources     map[string][]ResourceIntent // by lease id
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
}

// BindingInfo reports one configured source of work in neutral terms.
// SourceBindingKey is the provider's own identity, versioned and opaque
// here; for a GitHub Actions binding it reads as scope, URL, runner
// group and scale set name, which is enough for an operator to tell two
// bindings apart without the adapter's table being consulted.
type BindingInfo struct {
	ID               int64
	TargetID         string
	ProviderKind     string
	SourceBindingKey string
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
			ID: r.ID, TargetID: r.TargetID, ProviderKind: r.ProviderKind,
			SourceBindingKey: r.SourceBindingKey,
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
	LeasedBy         string
	LastUsed         int64
}

// Snapshot collects the instance's whole durable picture in one
// transaction, so what it reports is internally consistent.
func (s *Store) Snapshot() (Snapshot, error) {
	snap := Snapshot{
		InstanceID: s.instanceID,
		Attempts:   map[string]Attempt{},
		Resources:  map[string][]ResourceIntent{},
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
				snap.Queued[b.ID] = int(queued)
			}
		}
		if snap.CacheLanes, err = tx.CacheLanes(); err != nil {
			return err
		}
		snap.Pressure, err = tx.Pressure()
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
// It matches the generated GetAttempt query's projection; the schema
// parity test is what keeps the two in step.
const selectAttempt = `SELECT id, delivery_id, binding_id, source_workload_key, tenant_key,
       project_key, state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at FROM assignment_attempts `

// attemptsOfLeases reads the attempt each lease serves in one query.
//
// One read per lease made `runpool status` cost two round trips per row
// on the single connection every lease transition also waits for, and a
// snapshot carries up to a hundred leases.
func (t *Tx) attemptsOfLeases(leases []Lease) (map[string]Attempt, error) {
	out := make(map[string]Attempt, len(leases))
	if len(leases) == 0 {
		return out, nil
	}
	byAttempt := make(map[string]string, len(leases)) // attempt id -> lease id
	args := make([]any, 0, len(leases))
	for _, l := range leases {
		byAttempt[l.AttemptID] = l.ID
		args = append(args, l.AttemptID)
	}
	rows, err := t.tx.Query(selectAttempt+
		`WHERE id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r sqlitedb.AssignmentAttempt
		if err := rows.Scan(&r.ID, &r.DeliveryID, &r.BindingID, &r.SourceWorkloadKey,
			&r.TenantKey, &r.ProjectKey, &r.State, &r.ExecutionEvidence, &r.Resolution,
			&r.ReviewReason, &r.ReviewedAt, &r.ReviewedBy, &r.ReceivedAt, &r.SettledAt); err != nil {
			return nil, err
		}
		out[byAttempt[r.ID]] = fromRow(r)
	}
	return out, rows.Err()
}

// resourcesOfLeases reads every lease's resource intents in one query,
// for the same reason attemptsOfLeases exists.
func (t *Tx) resourcesOfLeases(leases []Lease) (map[string][]ResourceIntent, error) {
	out := make(map[string][]ResourceIntent, len(leases))
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
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
