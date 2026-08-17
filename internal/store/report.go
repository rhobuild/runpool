package store

import "context"

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
	DesiredState     string
	// Contact is what this binding's loop last managed with its provider.
	// A binding that has never run carries the zero value, which is not
	// the same as one that is failing: the first has nothing to report,
	// the second has a reason.
	Contact ProviderContact
}

// Bindings lists every recorded binding, in a stable order.
func (t *Tx) Bindings() ([]BindingInfo, error) {
	rows, err := t.q.ListProviderBindings(t.ctx)
	if err != nil {
		return nil, err
	}
	contacts, err := t.ProviderContacts()
	if err != nil {
		return nil, err
	}
	out := make([]BindingInfo, len(rows))
	for i, r := range rows {
		out[i] = BindingInfo{
			ID: r.ID, TargetID: r.TargetID, ProviderKind: r.ProviderKind,
			SourceBindingKey: r.SourceBindingKey, DesiredState: r.DesiredState,
			Contact: contacts[r.ID],
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
		for _, lease := range snap.Leases {
			attempt, err := tx.Get(lease.AttemptID)
			if err != nil {
				return err
			}
			snap.Attempts[lease.ID] = attempt

			resources, err := tx.Resources(lease.ID)
			if err != nil {
				return err
			}
			if len(resources) > 0 {
				snap.Resources[lease.ID] = resources
			}
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
