package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"
)

// PressureVerdict is one disk measurement to persist. An unknown verdict uses
// -1 for facts the failed measurement could not provide.
type PressureVerdict struct {
	Level        string
	FreeBytes    int64
	FreeInodes   int64
	ManagedBytes int64
}

// PressureInfo is the disk monitor's last persisted verdict. It is durable so
// status sees the level in force and a restarting controller preserves an
// emergency instead of assuming normal. MeasuredAt belongs to the stored fact,
// not to the write request: SQLite records it atomically with the verdict.
type PressureInfo struct {
	PressureVerdict
	MeasuredAt time.Time
}

// SetPressure records the monitor's verdict, replacing the previous one.
func (t *Tx) SetPressure(p PressureVerdict) error {
	return t.q.UpsertPressure(t.ctx, sqlitedb.UpsertPressureParams{
		Level: p.Level, FreeBytes: p.FreeBytes, FreeInodes: p.FreeInodes,
		ManagedBytes: p.ManagedBytes,
	})
}

// Pressure returns the last verdict, or nil when the monitor has never
// run — which callers must treat as "unknown", not "normal".
func (t *Tx) Pressure() (*PressureInfo, error) {
	r, err := t.q.GetPressure(t.ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &PressureInfo{
		PressureVerdict: PressureVerdict{
			Level: r.Level, FreeBytes: r.FreeBytes, FreeInodes: r.FreeInodes,
			ManagedBytes: r.ManagedBytes,
		},
		MeasuredAt: unixTime(r.MeasuredAt),
	}, nil
}

// AuditEntry is one maintenance action against a durable resource that
// no attempt can carry: who did it, what they did, to what.
type AuditEntry struct {
	ID      int64
	At      time.Time
	Actor   string
	Action  string
	Subject string
	Detail  string
}

// RecordAudit appends to the maintenance audit trail.
func (t *Tx) RecordAudit(actor, action, subject, detail string) error {
	return t.q.InsertAuditEntry(t.ctx, sqlitedb.InsertAuditEntryParams{
		Actor: actor, Action: action, Subject: subject, Detail: detail,
	})
}

// AuditTail returns the most recent entries, newest first.
func (t *Tx) AuditTail(limit int) ([]AuditEntry, error) {
	rows, err := t.q.ListAuditTail(t.ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	out := make([]AuditEntry, len(rows))
	for i, r := range rows {
		out[i] = AuditEntry{
			ID: r.ID, At: unixTime(r.At), Actor: r.Actor,
			Action: r.Action, Subject: r.Subject, Detail: r.Detail,
		}
	}
	return out, nil
}
