package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreMigrationBackupsAreNeverOverwritten: the backup is the
// documented rollback path, so a repeated or retried upgrade must preserve
// every earlier recovery point.
func TestPreMigrationBackupsAreNeverOverwritten(t *testing.T) {
	dir := t.TempDir()

	first, err := uniqueBackupPath(dir, 9)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "pre-migration-v9.db" {
		t.Errorf("first backup = %q; want the plain versioned name", filepath.Base(first))
	}
	if err := os.WriteFile(first, []byte("the copy that matters"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := uniqueBackupPath(dir, 9)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("a second backup would have overwritten the first")
	}
	if !strings.Contains(filepath.Base(second), "v9") {
		t.Errorf("second backup %q does not say which version it holds", filepath.Base(second))
	}

	body, err := os.ReadFile(first)
	if err != nil || string(body) != "the copy that matters" {
		t.Errorf("the first backup was disturbed: %q, %v", body, err)
	}
}

// TestUnknownSchemaVersionIsRefusedWithInstructions: a database from a
// build this one does not know is not repaired. The refusal has to tell
// the operator what to do, because that is the whole value of stopping.
func TestUnknownSchemaVersionIsRefusedWithInstructions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	// Claim a version far beyond anything this build ships.
	if _, err := st.db.Exec(`PRAGMA user_version = 9999`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	_, err = Open(dir, DefaultRetryBudget)
	if err == nil {
		t.Fatal("a database from an unknown build was opened")
	}
	msg := err.Error()
	for _, want := range []string{"9999", "does not repair", "export", "remove the state directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %s", want, msg)
		}
	}
}
