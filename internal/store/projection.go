package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// SchemaProjection applies every migration to a fresh database under dir
// and returns the normalized schema: table and index DDL as SQLite
// stores it, ordered deterministically, with a generated header.
//
// The migrations stay the single authority; this exists only to make
// them legible to sqlc, which needs one DDL file rather than a
// directory of forward steps. Two invocations over the same migrations
// yield identical bytes, which is what lets a test compare this against
// the committed snapshot and fail the build when the snapshot drifts.
func SchemaProjection(dir string) (string, error) {
	st, err := Open(dir)
	if err != nil {
		return "", fmt.Errorf("apply migrations: %w", err)
	}
	version, err := st.SchemaVersion()
	if err != nil {
		st.Close()
		return "", err
	}
	if err := st.Close(); err != nil {
		return "", err
	}

	// Plain read-only connection: the dump must see exactly what the
	// migrations committed, with no session pragmas of its own.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, DatabaseFile)+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT type, name, sql FROM sqlite_schema
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "-- Code generated from internal/store/migrations (schema version %d). DO NOT EDIT.\n", version)
	b.WriteString("-- Regenerate with: go run ./internal/store/schema/gen\n")
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			return "", err
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(ddl))
		b.WriteString(";\n")
	}
	return b.String(), rows.Err()
}
