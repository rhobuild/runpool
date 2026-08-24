// Package sqlitecontract is the durability contract for the pinned
// modernc.org/sqlite driver. The suite qualifies the exact driver version
// on the same kind of filesystem path production uses — a Linux Docker
// named volume — before any durable lifecycle state is built on it.
//
// The tests are gated: without RUNPOOL_SQLITE_CONTRACT_DIR they skip, so
// `go test ./...` stays fast and hermetic. scripts/contracts/sqlite.sh
// runs the full qualification (local race pass + Linux named-volume pass
// with container-kill rounds).
package sqlitecontract

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/rhobuild/runpool/internal/store"
)

const (
	envContractDir = "RUNPOOL_SQLITE_CONTRACT_DIR"
	envSmallDir    = "RUNPOOL_SQLITE_SMALL_DIR"
	envWriter      = "RUNPOOL_SQLITE_WRITER"
	envWriterDB    = "RUNPOOL_SQLITE_DB"
	envWriterLog   = "RUNPOOL_SQLITE_LOG"
	envWriterCount = "RUNPOOL_SQLITE_COUNT"
	envVerifyDB    = "RUNPOOL_SQLITE_VERIFY_DB"
	envVerifyLog   = "RUNPOOL_SQLITE_VERIFY_LOG"
)

// TestMain doubles as the crash-victim writer: with RUNPOOL_SQLITE_WRITER
// set, the test binary re-executes as a transaction writer that the tests
// (or the remote harness) terminate with SIGKILL.
func TestMain(m *testing.M) {
	if os.Getenv(envWriter) != "" {
		runWriter()
		return
	}
	os.Exit(m.Run())
}

func contractDir(t *testing.T) string {
	dir := os.Getenv(envContractDir)
	if dir == "" {
		t.Skipf("%s not set; the durability suite runs via scripts/contracts/sqlite.sh", envContractDir)
	}
	return dir
}

func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", store.DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// The production state store is a single writer; the contention test
	// opens its own additional handles explicitly.
	db.SetMaxOpenConns(1)
	return db
}

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	n      INTEGER PRIMARY KEY,
	filler TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v INTEGER NOT NULL
);`

// runWriter appends numbered entries in one multi-statement transaction
// each (entry + meta counter), mimicking a lease state transition. Every
// confirmed commit is echoed to the log file after the commit returns, so
// the database may contain more than the log ever records, never less.
func runWriter() {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "writer:", err)
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", store.DSN(os.Getenv(envWriterDB)))
	if err != nil {
		fail(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		fail(err)
	}

	var log *os.File
	if path := os.Getenv(envWriterLog); path != "" {
		if log, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
			fail(err)
		}
	}
	limit, _ := strconv.Atoi(os.Getenv(envWriterCount)) // 0 = run until killed

	filler := strings.Repeat("x", 200)
	for i := 0; limit == 0 || i < limit; i++ {
		tx, err := db.Begin()
		if err != nil {
			fail(err)
		}
		var next int64
		if err := tx.QueryRow(`SELECT coalesce(max(n), 0) + 1 FROM entries`).Scan(&next); err != nil {
			fail(err)
		}
		if _, err := tx.Exec(`INSERT INTO entries (n, filler) VALUES (?, ?)`, next, filler); err != nil {
			fail(err)
		}
		if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES ('last', ?)
			ON CONFLICT (k) DO UPDATE SET v = excluded.v`, next); err != nil {
			fail(err)
		}
		if err := tx.Commit(); err != nil {
			fail(err)
		}
		if log != nil {
			fmt.Fprintf(log, "%d\n", next)
		}
	}
	os.Exit(0)
}

// verifyDB checks every durability invariant a recovered database must
// hold: it passes integrity_check, the entry sequence has no holes or
// torn rows, the meta counter matches the last entry (multi-statement
// atomicity), and it contains at least every commit the log confirmed.
func verifyDB(dbPath, logPath string) error {
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		return err
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity_check: %s", integrity)
	}

	var count, max, last int64
	if err := db.QueryRow(`SELECT count(*), coalesce(max(n), 0) FROM entries`).Scan(&count, &max); err != nil {
		return err
	}
	if count != max {
		return fmt.Errorf("entry sequence has holes: count=%d max=%d", count, max)
	}
	if err := db.QueryRow(`SELECT coalesce((SELECT v FROM meta WHERE k = 'last'), 0)`).Scan(&last); err != nil {
		return err
	}
	if last != max {
		return fmt.Errorf("transaction atomicity broken: meta=%d max=%d", last, max)
	}

	if logPath != "" {
		confirmed, err := lastConfirmed(logPath)
		if err != nil {
			return err
		}
		if max < confirmed {
			return fmt.Errorf("durability broken: log confirmed %d but database holds %d", confirmed, max)
		}
	}
	return nil
}

// lastConfirmed returns the highest fully written line of the log; a
// final line torn by SIGKILL is ignored, which only weakens the check.
func lastConfirmed(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	var confirmed int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if n, err := strconv.ParseInt(strings.TrimSpace(sc.Text()), 10, 64); err == nil && n > confirmed {
			confirmed = n
		}
	}
	return confirmed, sc.Err()
}

// TestVerifyExisting lets the remote harness verify a database written by
// a container the harness killed; it is skipped everywhere else.
func TestVerifyExisting(t *testing.T) {
	dbPath := os.Getenv(envVerifyDB)
	if dbPath == "" {
		t.Skipf("%s not set", envVerifyDB)
	}
	if err := verifyDB(dbPath, os.Getenv(envVerifyLog)); err != nil {
		t.Fatal(err)
	}
	max := queryInt(t, open(t, dbPath), `SELECT coalesce(max(n), 0) FROM entries`)
	if max == 0 {
		t.Fatal("the recovered database holds no committed work: the writer was killed before " +
			"it started, so this round verified recovery of nothing")
	}
	t.Logf("verified %s: %d entries survived a container kill", filepath.Base(dbPath), max)
}

func queryInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
