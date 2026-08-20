package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaIdentifiedByContentsNotCount is the regression test for a
// database this build cannot account for while the baseline is still
// mutable.
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
		if strings.Contains(err.Error(), "no such table") {
			t.Errorf("error = %q; the raw error is what this check exists to replace", err)
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
