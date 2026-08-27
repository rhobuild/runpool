package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaIdentifiedByContentsNotCount is the regression test for a
// database this build cannot account for because a migration was edited
// rather than added.
//
// PRAGMA user_version counts migrations, so editing the single reviewed
// baseline in place leaves an older database reporting the current
// version while holding different tables. Both guards then passed it and
// the first query failed with the raw SQLite error they exist to replace.
func TestSchemaIdentifiedByContentsNotCount(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Logf("note: %d migrations; this test is about a schema whose version cannot move", len(migrations))
	}

	// A database at the current version, applied from a different
	// baseline: the same shape an in-place edit leaves behind.
	dir := t.TempDir()
	seed := migrations[len(migrations)-1]
	seed.up = strings.Replace(migrations[len(migrations)-1].up,
		"CREATE TABLE meta (", "CREATE TABLE unrelated (x INTEGER);\nCREATE TABLE meta (", 1)
	if seed.up == migrations[len(migrations)-1].up {
		t.Fatal("could not build a differing baseline; the anchor moved")
	}
	older := append(append([]migration{}, migrations[:len(migrations)-1]...), seed)

	s := openRaw(t, dir)
	for _, m := range older {
		stamp := ""
		if m.version == len(older) {
			stamp = schemaFingerprint(older)
		}
		if err := s.applyScript(m.up, m.version, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	t.Run("the controller refuses it", func(t *testing.T) {
		_, err := Open(dir, DefaultRetryBudget)
		if err == nil {
			t.Fatal("a database written from different migrations was opened")
		}
		if !strings.Contains(err.Error(), "different migrations") {
			t.Errorf("error = %q; want the schema refusal, not a raw SQLite error", err)
		}
	})

	t.Run("reporting refuses it too", func(t *testing.T) {
		_, err := OpenReadOnly(dir)
		if err == nil {
			t.Fatal("a database written from different migrations was opened for reporting")
		}
		if !strings.Contains(err.Error(), "different migrations") {
			t.Errorf("error = %q; want the schema refusal itself -- excluding the raw error also "+
				"accepted any unrelated failure, which is not this check working", err)
		}
	})

	t.Run("a database with no fingerprint at all is refused", func(t *testing.T) {
		bare := t.TempDir()
		b := openRaw(t, bare)
		for _, m := range migrations {
			if err := b.applyScript(m.up, m.version, ""); err != nil {
				t.Fatal(err)
			}
		}
		b.Close()
		if _, err := Open(bare, DefaultRetryBudget); err == nil {
			t.Fatal("a database predating the fingerprint was opened")
		}
	})
}

// TestAStoreThisBuildWroteReopens holds the other side: the refusal must
// not be reachable from the ordinary path, or it would refuse everything.
func TestAStoreThisBuildWroteReopens(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir)
	id := first.InstanceID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, DefaultRetryBudget)
	if err != nil {
		t.Fatalf("reopening a store this build created: %v", err)
	}
	defer again.Close()
	if again.InstanceID() != id {
		t.Errorf("instance id = %q, want %q", again.InstanceID(), id)
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("reporting on a store this build created: %v", err)
	}
	ro.Close()
}

// openRaw opens the database without migrating it, so a test can apply a
// baseline of its own choosing.
func openRaw(t *testing.T, dir string) *Store {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", DSN(filepath.Join(dir, DatabaseFile)))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db, dir: dir, retryBudget: DefaultRetryBudget}
}

// TestAnEditedMigrationBelowAPendingOneIsRefused: pending migrations are
// no exemption from the identity check. Verified only when nothing was
// pending, an edited migration below the pending one slipped through --
// the upgrade applied, the edited set's fingerprint was stamped, and
// every later open verified clean against a database the edit never
// reached. The first query touching the difference then failed with the
// raw error the fingerprint exists to replace.
func TestAnEditedMigrationBelowAPendingOneIsRefused(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seed := migrations[len(migrations)-1]
	seed.up = strings.Replace(seed.up,
		"CREATE TABLE meta (", "CREATE TABLE unrelated (x INTEGER);\nCREATE TABLE meta (", 1)
	older := append(append([]migration{}, migrations[:len(migrations)-1]...), seed)

	s := openRaw(t, dir)
	for _, m := range older {
		stamp := ""
		if m.version == len(older) {
			stamp = schemaFingerprint(older)
		}
		if err := s.applyScript(m.up, m.version, stamp); err != nil {
			t.Fatal(err)
		}
	}
	defer s.Close()

	pending := append(append([]migration{}, migrations...), migration{
		version: len(migrations) + 1,
		name:    "synthetic",
		up:      `CREATE TABLE synthetic_two (id INTEGER PRIMARY KEY);`,
	})
	err = s.applyMigrations(pending)
	if err == nil {
		t.Fatal("a pending migration was applied on top of a prefix another build wrote")
	}
	if !strings.Contains(err.Error(), "different migrations") {
		t.Errorf("error = %q; want the schema refusal", err)
	}
	if v, _ := s.SchemaVersion(); v != len(older) {
		t.Errorf("schema version = %d after the refusal; the pending migration must not have applied", v)
	}
}

// TestAnUpgradeInterruptedMidSequenceResumes: each migration commits the
// fingerprint of the set up to and including itself, in its own
// transaction. Stamped only with the last, a crash between two pending
// migrations left a database whose applied prefix had grown while its
// recorded identity had not — and the resume's own identity check then
// refused the database it was resuming.
func TestAnUpgradeInterruptedMidSequenceResumes(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := openRaw(t, dir)
	defer s.Close()
	if err := s.applyMigrations(migrations); err != nil {
		t.Fatal(err)
	}

	good := migration{version: len(migrations) + 1, name: "first",
		up: `CREATE TABLE synthetic_one (id INTEGER PRIMARY KEY);`}
	bad := migration{version: len(migrations) + 2, name: "second",
		up: `THIS IS NOT SQL;`}
	if err := s.applyMigrations(append(append([]migration{}, migrations...), good, bad)); err == nil {
		t.Fatal("a migration that is not SQL applied")
	}
	// The crash-shaped state the failure leaves: the first pending
	// migration committed, the second did not.
	if v, _ := s.SchemaVersion(); v != len(migrations)+1 {
		t.Fatalf("schema version = %d; want %d — each migration commits alone", v, len(migrations)+1)
	}

	fixed := migration{version: len(migrations) + 2, name: "second",
		up: `CREATE TABLE synthetic_two (id INTEGER PRIMARY KEY);`}
	if err := s.applyMigrations(append(append([]migration{}, migrations...), good, fixed)); err != nil {
		t.Fatalf("resuming an interrupted upgrade: %v", err)
	}
	if v, _ := s.SchemaVersion(); v != len(migrations)+2 {
		t.Errorf("schema version = %d; want %d", v, len(migrations)+2)
	}
}
