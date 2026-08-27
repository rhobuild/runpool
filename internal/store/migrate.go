package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed migrations
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	up      string
}

// loadMigrations reads the embedded set once. Every open consults it and
// the bytes cannot change while the process runs.
var loadMigrations = sync.OnceValues(embeddedMigrations)

// embeddedMigrations reads migrations/NNNNNN_name.up.sql in order.
//
// There are no down scripts. A down script claims every schema change is
// losslessly reversible, which is false the moment one drops a column;
// the rollback path is restoring the backup taken before the migration
// ran, which is a copy of what actually existed rather than a guess at
// how to rebuild it.
func embeddedMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue // editor and platform metadata, never a migration
		}
		base, ok := strings.CutSuffix(name, ".up.sql")
		if !ok {
			return nil, fmt.Errorf("migration %q: expected NNNNNN_name.up.sql", name)
		}
		versionText, title, _ := strings.Cut(base, "_")
		version, err := strconv.Atoi(versionText)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %q: invalid version", name)
		}
		// Redundant as a detector -- the contiguity check below fails on
		// any duplicate too -- and kept for its message: this one names
		// both files, where that one reports a position.
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, name, version)
		}
		seen[version] = name
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: title, up: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 1; found %d at position %d", m.version, i+1)
		}
	}
	return out, nil
}

// schemaKey is where the applied schema's fingerprint is recorded, in the
// same transaction that applies it.
const schemaKey = "schema_fingerprint"

// schemaFingerprint identifies a schema by what it says rather than by how
// many files say it.
//
// PRAGMA user_version counts migrations, which is the right identity for a
// schema that only ever grows by adding one. It is the wrong identity for
// a baseline edited in place: the count does not move, so a database
// written by an earlier build reports the same version while holding
// different tables.
// Both guards below then pass it, and the first query fails with the raw
// SQLite error they exist to replace.
//
// The fingerprint covers each migration's version, name and body, so any
// edit to any of them is a different schema by definition.
func schemaFingerprint(migrations []migration) string {
	h := sha256.New()
	for _, m := range migrations {
		fmt.Fprintf(h, "%06d\x00%s\x00%d\x00", m.version, m.name, len(m.up))
		h.Write([]byte(m.up))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newerSchemaError refuses a database this build cannot interpret,
// naming the recovery options instead of attempting an implicit repair.
//
// One function because every path that opens a database owes the operator
// the same answer: a reporting command that met a newer schema and
// returned a raw "no such table" would be repairing nothing and
// explaining nothing.
func newerSchemaError(current, known int, dir string) error {
	return fmt.Errorf(
		"state schema is version %d but this build knows only %d.\n"+
			"This database was written by a different build. Runpool does not repair "+
			"a schema it cannot account for.\n"+
			"  - to keep the data: run the build that wrote it and export what you need\n"+
			"  - to start clean:   stop the controller and remove the state directory %s",
		current, known, dir)
}

// alteredSchemaError refuses a database whose schema is this build's
// version but not this build's schema. It is reachable when a migration
// is edited rather than added: a schema that grows by adding one moves
// the version and takes the other path.
func alteredSchemaError(dir string) error {
	return fmt.Errorf(
		"state schema is version-compatible with this build but was written from different migrations.\n"+
			"Runpool identifies a schema by its contents, so it will not read one it cannot account for.\n"+
			"  - to keep the data: run the build that wrote it and export what you need\n"+
			"  - to start clean:   stop the controller and remove the state directory %s",
		dir)
}

// applyMigrations moves the schema forward to the newest version. Each
// migration commits its DDL together with PRAGMA user_version and the
// schema fingerprint in one transaction — the mechanism the durability
// suite qualifies — and a schema that already holds data is backed up
// before the first pending migration touches it.
func (s *Store) applyMigrations(migrations []migration) error {
	current, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return newerSchemaError(current, len(migrations), s.dir)
	}
	if current > 0 {
		// The prefix already applied has to be this build's prefix, and
		// pending migrations are no exemption from that. Checked only
		// when nothing was pending, an edited migration below the
		// pending one slipped through: the upgrade applied, the edited
		// set's fingerprint was stamped, and every later open verified
		// clean against a database whose contents the edit never
		// reached. The first query touching the difference then failed
		// with a raw error -- precisely the outcome alteredSchemaError
		// exists to replace.
		if err := s.verifyFingerprint(migrations[:current]); err != nil {
			return err
		}
	}
	if current == len(migrations) {
		return nil
	}

	if current > 0 {
		// The backup is the rollback path — restoring it is how a
		// failed upgrade is undone — so it is never overwritten. Retries
		// receive a new path and preserve every earlier recovery point.
		backup, err := uniqueBackupPath(s.dir, current)
		if err != nil {
			return err
		}
		if err := s.Backup(context.Background(), backup); err != nil {
			return err
		}
	}

	for _, m := range migrations[current:] {
		// Each migration stamps the fingerprint of the set up to and
		// including itself, in its own transaction: a schema and its
		// identity commit together or neither does, so a crash anywhere
		// in the sequence leaves a database whose recorded identity is
		// exactly its applied prefix — which is what the check above
		// verifies when the upgrade resumes.
		if err := s.applyScript(m.up, m.version, schemaFingerprint(migrations[:m.version])); err != nil {
			return fmt.Errorf("migration %06d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// verifyFingerprint compares the recorded schema identity with this
// build's. A database that predates the fingerprint has none recorded,
// and is refused for the same reason as one that disagrees: this build
// cannot show that it knows what is in there.
func (s *Store) verifyFingerprint(migrations []migration) error {
	var recorded string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, schemaKey).Scan(&recorded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return alteredSchemaError(s.dir)
	case err != nil:
		return err
	case recorded != schemaFingerprint(migrations):
		return alteredSchemaError(s.dir)
	}
	return nil
}

// uniqueBackupPath names a pre-migration backup that cannot collide
// with an earlier one. The version says what the copy contains; the
// counter keeps a retried or repeated upgrade from destroying it.
func uniqueBackupPath(dir string, version int) (string, error) {
	for n := 0; n < 1000; n++ {
		name := fmt.Sprintf("pre-migration-v%d.db", version)
		if n > 0 {
			name = fmt.Sprintf("pre-migration-v%d.%d.db", version, n)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("cannot name a pre-migration backup in %s: a thousand already exist", dir)
}

func (s *Store) applyScript(script string, resultVersion int, fingerprint string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(script); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, resultVersion)); err != nil {
		tx.Rollback()
		return err
	}
	if fingerprint != "" {
		if _, err := tx.Exec(
			`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
			schemaKey, fingerprint); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
