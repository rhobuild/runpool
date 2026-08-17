package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenReadOnlyCreatesNothing prevents reporting from opening the
// writable DSN and creating a database it was only meant to read.
func TestOpenReadOnlyCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(dir); !errors.Is(err, ErrNoState) {
		t.Fatalf("OpenReadOnly on an empty directory = %v; want ErrNoState", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("reporting created %d files in an empty state directory", len(entries))
	}
}

// TestReadOnlyStoreCannotWrite proves the guarantee is enforced by the
// engine rather than by convention: a write through a reporting store
// fails even if some future caller attempts one.
func TestReadOnlyStoreCannotWrite(t *testing.T) {
	dir := t.TempDir()
	writable := openStore(t, dir)
	seedLease(t, writable, "readonly")
	writable.Close()

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	if _, err := ro.Snapshot(); err != nil {
		t.Fatalf("reading through a read-only store: %v", err)
	}
	err = ro.Tx(t.Context(), func(tx *Tx) error {
		return tx.RecordAudit("test", "probe", "instance", "a write that must not land")
	})
	if err == nil {
		t.Error("a write through the read-only store succeeded")
	}
}

// TestReadOnlyLeavesFilesUntouched checks the property an operator cares
// about: running status against a live controller's store changes
// nothing on disk.
func TestReadOnlyLeavesFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	writable := openStore(t, dir)
	seedLease(t, writable, "readonly")
	writable.Close()

	before := digests(t, dir)
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Snapshot(); err != nil {
		t.Fatal(err)
	}
	ro.Close()

	after := digests(t, dir)
	if before[DatabaseFile] != after[DatabaseFile] {
		t.Error("the database changed during a read-only open")
	}
	for name, sum := range before {
		if after[name] != sum {
			t.Errorf("%s changed during a read-only open", name)
		}
	}
}

// digests hashes the database file. The -wal and -shm side files are
// excluded because SQLite materializes and maps them for any reader of a
// WAL database whose log was checkpointed away — reading cannot avoid
// that, short of an immutable database or a read-only directory.
//
// SQLite readers may create WAL side files, so byte identity for DB, WAL,
// and SHM together is not a valid read-only contract. What this asserts is
// that no data changes: the database is
// byte-identical, and the store cannot write (TestReadOnlyStoreCannotWrite
// proves the engine refuses).
func digests(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][32]byte{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-shm") || strings.HasSuffix(e.Name(), "-wal") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = sha256.Sum256(data)
	}
	return out
}

// TestOpenReadOnlyRefusesASchemaItCannotReport pins the answer a reporting
// command owes an operator whose database is not the shape this build
// knows. Applying no migrations means the schema has to be checked, in
// both directions: a newer one follows a downgrade, an older one lives in
// the window between installing a build and restarting the controller.
func TestOpenReadOnlyRefusesASchemaItCannotReport(t *testing.T) {
	migrations, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	known := len(migrations)

	for name, tc := range map[string]struct {
		version int
		want    string
	}{
		"newer than this build": {known + 1, "knows only"},
		"older than this build": {known - 1, "predates this build"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir)
			if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", tc.version)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			ro, err := OpenReadOnly(dir)
			if err == nil {
				ro.Close()
				t.Fatalf("a version %d schema was opened for reporting by a build that knows %d",
					tc.version, known)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestASchemaBehindIsNamed: a preview reads and the applying form
// migrates, so the safe command is the one a not-yet-migrated schema
// closes. The caller has to be able to say that, which means telling this
// refusal apart from every other one.
func TestASchemaBehindIsNamed(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := openStore(t, dir)
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)-1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenReadOnly(dir)
	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("error = %v; want it to be recognisable as ErrSchemaBehind", err)
	}
}
