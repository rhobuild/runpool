package store

import (
	"database/sql"
	"errors"
)

// PressureInfo is the disk monitor's last persisted verdict. It is
// durable so `runpool status` sees the level in force and a restarting
// controller resumes from it instead of assuming normal — an emergency
// must survive the process that declared it.
type PressureInfo struct {
	Level        string
	FreeBytes    int64
	FreeInodes   int64
	ManagedBytes int64
	MeasuredAt   int64
}

// SetPressure records the monitor's verdict, replacing the previous one.
func (t *Tx) SetPressure(p PressureInfo) error {
	_, err := t.tx.Exec(`
		INSERT INTO pressure (id, level, free_bytes, free_inodes, managed_bytes, measured_at)
		VALUES (1, ?, ?, ?, ?, unixepoch())
		ON CONFLICT (id) DO UPDATE SET
			level = excluded.level, free_bytes = excluded.free_bytes,
			free_inodes = excluded.free_inodes, managed_bytes = excluded.managed_bytes,
			measured_at = excluded.measured_at`,
		p.Level, p.FreeBytes, p.FreeInodes, p.ManagedBytes)
	return err
}

// Pressure returns the last verdict, or nil when the monitor has never
// run — which callers must treat as "unknown", not "normal".
func (t *Tx) Pressure() (*PressureInfo, error) {
	var p PressureInfo
	err := t.tx.QueryRow(`
		SELECT level, free_bytes, free_inodes, managed_bytes, measured_at
		FROM pressure WHERE id = 1`).
		Scan(&p.Level, &p.FreeBytes, &p.FreeInodes, &p.ManagedBytes, &p.MeasuredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// AuditEntry is one maintenance action against a durable resource that
// no attempt can carry: who did it, what they did, to what.
type AuditEntry struct {
	ID      int64
	At      int64
	Actor   string
	Action  string
	Subject string
	Detail  string
}

// RecordAudit appends to the maintenance audit trail.
func (t *Tx) RecordAudit(actor, action, subject, detail string) error {
	_, err := t.tx.Exec(`
		INSERT INTO audit_log (actor, action, subject, detail) VALUES (?, ?, ?, ?)`,
		actor, action, subject, detail)
	return err
}

// AuditTail returns the most recent entries, newest first.
func (t *Tx) AuditTail(limit int) ([]AuditEntry, error) {
	rows, err := t.tx.Query(`
		SELECT id, at, actor, action, subject, detail
		FROM audit_log ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
