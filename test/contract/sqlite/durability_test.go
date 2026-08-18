package sqlitecontract

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPragmas proves the DSN applies the durability configuration to a
// fresh connection and that foreign keys are actually enforced, not just
// reported enabled.
func TestPragmas(t *testing.T) {
	db := open(t, filepath.Join(contractDir(t), "pragmas.db"))

	var sqliteVersion, journal string
	var sync, fk, busy int64
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		t.Fatal(err)
	}
	t.Logf("embedded sqlite %s", sqliteVersion)
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q; want wal", journal)
	}
	if sync = queryInt(t, db, `PRAGMA synchronous`); sync != 2 {
		t.Errorf("synchronous = %d; want 2 (FULL)", sync)
	}
	if fk = queryInt(t, db, `PRAGMA foreign_keys`); fk != 1 {
		t.Errorf("foreign_keys = %d; want 1", fk)
	}
	if busy = queryInt(t, db, `PRAGMA busy_timeout`); busy != 10000 {
		t.Errorf("busy_timeout = %d; want 10000", busy)
	}

	mustExec(t, db, `CREATE TABLE IF NOT EXISTS parent (id INTEGER PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS child (pid INTEGER NOT NULL REFERENCES parent (id))`)
	if _, err := db.Exec(`INSERT INTO child (pid) VALUES (42)`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("orphan insert error = %v; want FOREIGN KEY violation", err)
	}
}

// TestCrashRecovery SIGKILLs a live writer twelve times against one
// database and requires a clean recovery with every confirmed commit
// present after each round.
func TestCrashRecovery(t *testing.T) {
	dir := contractDir(t)
	dbPath := filepath.Join(dir, "crash.db")
	logPath := filepath.Join(dir, "crash.log")

	// The database is established before the first victim starts, for
	// the same reason TestContention establishes it: the controller
	// creates its database under the flock singleton before anything
	// else can reach it, so the crash production can deal a writer is
	// against an established WAL database. Without this, a kill that
	// lands inside the victim's own schema bootstrap leaves no entries
	// table — correct durability for an uncommitted CREATE TABLE, but
	// the verifier reads the absence as a lost table. DDL atomicity has
	// its own coverage in TestMigrationMechanics.
	setup := open(t, dbPath)
	mustExec(t, setup, schema)
	setup.Close()

	for round := range 12 {
		cmd := writerCmd(dbPath, logPath, 0)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(40+rand.Intn(360)) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		if err := verifyDB(dbPath, logPath); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	db := open(t, dbPath)
	t.Logf("recovered cleanly 12 times; %d entries survived", queryInt(t, db, `SELECT count(*) FROM entries`))
}

// TestContention runs three writer processes against one database; with
// immediate transactions and a shared busy timeout every transaction must
// apply exactly once, with no holes and no unhandled SQLITE_BUSY.
func TestContention(t *testing.T) {
	dbPath := filepath.Join(contractDir(t), "contention.db")
	const writers, each = 3, 200

	// The database is established before anyone contends. Production
	// never lets two processes race the initial WAL conversion — the
	// controller creates its database under the flock singleton before
	// any other process can reach it — and a three-way race to convert a
	// fresh file exercises an open-time interlock SQLite resolves by
	// returning SQLITE_BUSY without consulting the busy handler. The
	// release contract is transaction contention on an
	// established WAL database, which is the only shape production has.
	setup := open(t, dbPath)
	mustExec(t, setup, schema)
	setup.Close()

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := writerCmd(dbPath, "", each).CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("writer: %v: %s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if err := verifyDB(dbPath, ""); err != nil {
		t.Fatal(err)
	}
	db := open(t, dbPath)
	if got := queryInt(t, db, `SELECT count(*) FROM entries`); got != writers*each {
		t.Errorf("entries = %d; want %d", got, writers*each)
	}
}

// TestSingletonLock models the active/standby handover: while one
// connection holds a write transaction, a second with a short busy
// timeout must fail fast with a busy error — never hang, never corrupt —
// and succeed once the holder commits.
func TestSingletonLock(t *testing.T) {
	dbPath := filepath.Join(contractDir(t), "lock.db")
	active := open(t, dbPath)
	mustExec(t, active, schema)

	standby, err := sql.Open("sqlite", "file:"+dbPath+"?_txlock=immediate&_pragma=journal_mode(wal)&_pragma=busy_timeout(500)")
	if err != nil {
		t.Fatal(err)
	}
	defer standby.Close()

	tx, err := active.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES ('lock', 1) ON CONFLICT (k) DO UPDATE SET v = v + 1`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = standby.Exec(`INSERT INTO meta (k, v) VALUES ('standby', 1)`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "busy") {
		t.Fatalf("standby write error = %v; want busy", err)
	}
	// The retry loop accounts accumulated backoff, not wall clock, so it
	// gives up somewhat before the nominal 500ms; the invariant is that
	// it neither fails instantly nor hangs.
	if waited := time.Since(start); waited < 200*time.Millisecond || waited > 5*time.Second {
		t.Errorf("standby waited %v; want roughly the 500ms busy timeout", waited)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := standby.Exec(`INSERT INTO meta (k, v) VALUES ('standby', 1) ON CONFLICT (k) DO UPDATE SET v = v + 1`); err != nil {
		t.Fatalf("standby write after handover: %v", err)
	}
}

// TestBackupRestore takes an online backup with VACUUM INTO while a
// writer keeps committing, then proves the copy is a consistent,
// restorable snapshot.
func TestBackupRestore(t *testing.T) {
	dir := contractDir(t)
	dbPath := filepath.Join(dir, "backup-src.db")
	backupPath := filepath.Join(dir, "backup-copy.db")
	_ = os.Remove(backupPath) // VACUUM INTO refuses to overwrite

	if out, err := writerCmd(dbPath, "", 100).CombinedOutput(); err != nil {
		t.Fatalf("seed writer: %v: %s", err, out)
	}

	done := make(chan error, 1)
	go func() {
		out, err := writerCmd(dbPath, "", 100).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("concurrent writer: %v: %s", err, out)
		}
		done <- err
	}()

	db := open(t, dbPath)
	if _, err := db.Exec(`VACUUM INTO '` + backupPath + `'`); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if err := verifyDB(backupPath, ""); err != nil {
		t.Fatalf("backup not consistent: %v", err)
	}
	restored := open(t, backupPath)
	got := queryInt(t, restored, `SELECT count(*) FROM entries`)
	if got < 100 || got > 200 {
		t.Errorf("backup snapshot holds %d entries; want a point between 100 and 200", got)
	}
	t.Logf("restorable backup verified with %d entries while a writer was live", got)
}

// TestDiskFull fills a capped filesystem until SQLITE_FULL, requires the
// error to be clean and the database to stay intact, then frees external
// space — the disk monitor's emergency evicts cache, never the state
// database — and requires writes to resume. On a 100%-full filesystem
// even a DELETE fails, because it too must write WAL pages first.
func TestDiskFull(t *testing.T) {
	small := os.Getenv(envSmallDir)
	if small == "" {
		t.Skipf("%s not set; disk-full runs inside the qualification container", envSmallDir)
	}
	ballast := filepath.Join(small, "ballast")
	if err := os.WriteFile(ballast, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	db := open(t, filepath.Join(small, "full.db"))
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS big (id INTEGER PRIMARY KEY, blob TEXT NOT NULL)`)

	blob := strings.Repeat("x", 64<<10)
	var writeErr error
	for range 4096 {
		if _, writeErr = db.Exec(`INSERT INTO big (blob) VALUES (?)`, blob); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		t.Fatal("filled 256MiB into a capped filesystem without an error; cap not effective")
	}
	if !strings.Contains(strings.ToLower(writeErr.Error()), "full") {
		t.Fatalf("disk-full error = %v; want SQLITE_FULL", writeErr)
	}

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity after disk-full = %q, %v", integrity, err)
	}

	if err := os.Remove(ballast); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DELETE FROM big WHERE id IN (SELECT id FROM big LIMIT 100)`)
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO big (blob) VALUES ('recovered')`); err != nil {
		t.Fatalf("write after freeing space: %v", err)
	}
}

// TestMigrationMechanics proves DDL plus user_version move atomically in
// one transaction, both forward and back — the mechanism the state
// store's migrations will rely on.
func TestMigrationMechanics(t *testing.T) {
	db := open(t, filepath.Join(contractDir(t), "migrate.db"))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE m1 (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if v := queryInt(t, db, `PRAGMA user_version`); v != 0 {
		t.Fatalf("user_version after rollback = %d; want 0", v)
	}
	if _, err := db.Exec(`INSERT INTO m1 (id) VALUES (1)`); err == nil {
		t.Fatal("table m1 exists after rollback")
	}

	apply := func(stmts []string, version int64) {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := tx.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if v := queryInt(t, db, `PRAGMA user_version`); v != version {
			t.Fatalf("user_version = %d; want %d", v, version)
		}
	}
	apply([]string{`CREATE TABLE m1 (id INTEGER PRIMARY KEY)`}, 1) // up
	apply([]string{`DROP TABLE m1`}, 0)                            // down
}

func writerCmd(dbPath, logPath string, count int) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		envWriter+"=1",
		envWriterDB+"="+dbPath,
		envWriterLog+"="+logPath,
		fmt.Sprintf("%s=%d", envWriterCount, count),
	)
	return cmd
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}
