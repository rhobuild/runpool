package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
)

// seedLease is the common starting point for the runtime-lifecycle
// tests: one binding, one delivered workload, one lease serving it.
func seedLease(t *testing.T, s *Store, key string) Lease {
	t.Helper()
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, assignment.DeliveryKey("msg-"+key),
		assignment.SourceWorkloadKey("job-"+key))
	var lease Lease
	inTx(t, s, func(tx *Tx) error {
		var err error
		lease, err = tx.LeaseAttempt(assignment.AttemptID(attempt), assignment.BindingID(binding), "standard")
		return err
	})
	return lease
}

func TestOpenCreatesSchemaAndStableIdentity(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	migrations, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.SchemaVersion(); err != nil || v != len(migrations) {
		t.Fatalf("schema version = %d, %v; want %d", v, err, len(migrations))
	}
	first := s.InstanceID()
	if len(first) != 32 {
		t.Fatalf("instance id = %q; want 32 hex chars", first)
	}
	s.Close()

	if again := openStore(t, dir).InstanceID(); again != first {
		t.Errorf("instance id changed across reopen: %q != %q", again, first)
	}
}

// The schema ships as one reviewed baseline, and a migration added to it
// is forward-only.
func TestSchemaShipsAsOneReviewedBaseline(t *testing.T) {
	migrations, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations are embedded")
	}
	if migrations[0].name != "initial" {
		t.Errorf("first migration is %q; want the initial baseline", migrations[0].name)
	}
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".sql" && !isUpScript(e.Name()) {
			t.Errorf("%s: there are no down scripts; restoring the pre-migration backup is the rollback", e.Name())
		}
	}
}

func isUpScript(name string) bool {
	return len(name) > 7 && name[len(name)-7:] == ".up.sql"
}

func TestSchemaAheadOfBuildFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(dir, DefaultRetryBudget); err == nil {
		t.Fatal("opening a newer schema succeeded; want refusal")
	}
}

func TestLockExcludesSecondHolder(t *testing.T) {
	dir := t.TempDir()
	lock, err := TryAcquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(dir); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second acquire = %v; want ErrLockHeld", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	third, err := TryAcquire(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	third.Release()
}

func TestLeaseLifecycleHappyPath(t *testing.T) {
	s := newStore(t)
	lease := seedLease(t, s, "happy")

	// A lease is born reserved: the credit is already held when the row is
	// written, so there is no earlier state a crash could leave behind.
	if lease.State != LeaseReserved {
		t.Fatalf("new lease state = %s; want reserved", lease.State)
	}

	path := []LeaseState{
		LeaseProvisioning, LeaseRuntimeRegistered,
		LeaseWorkloadRunning, LeaseDraining, LeaseCleaning, LeaseReleased,
	}
	from := LeaseReserved
	for _, to := range path {
		inTx(t, s, func(tx *Tx) error { return tx.TransitionLease(lease.ID, from, to) })
		from = to
	}

	inTx(t, s, func(tx *Tx) error {
		got, err := tx.LeaseByID(lease.ID)
		if err != nil {
			return err
		}
		if got.State != LeaseReleased {
			t.Errorf("final state = %s; want released", got.State)
		}
		if !got.State.Terminal() {
			t.Error("released must be terminal")
		}
		return nil
	})
}

// TestFailureAndQuarantinePath walks the way out of a failure: a lease
// that could not be cleaned parks in quarantine, holding its admission
// credit, and a later retry has to be able to finish it. Each step is
// checked where it lands, because a machine that accepts a transition
// without making it would pass a test that only watched for errors — and
// a lease left short of released holds that credit forever.
func TestFailureAndQuarantinePath(t *testing.T) {
	s := newStore(t)
	lease := seedLease(t, s, "quarantine")

	steps := [][2]LeaseState{
		{LeaseReserved, LeaseProvisioning},
		{LeaseProvisioning, LeaseFailed},
		{LeaseFailed, LeaseCleaning},
		{LeaseCleaning, LeaseQuarantined},
		{LeaseQuarantined, LeaseCleaning},
		{LeaseCleaning, LeaseReleased},
	}
	for _, step := range steps {
		inTx(t, s, func(tx *Tx) error { return tx.TransitionLease(lease.ID, step[0], step[1]) })
		inTx(t, s, func(tx *Tx) error {
			got, err := tx.LeaseByID(lease.ID)
			if err != nil {
				return err
			}
			if got.State != step[1] {
				t.Errorf("after %s -> %s the lease is %s", step[0], step[1], got.State)
			}
			return nil
		})
	}
	inTx(t, s, func(tx *Tx) error {
		got, err := tx.LeaseByID(lease.ID)
		if err != nil {
			return err
		}
		if !got.State.Terminal() {
			t.Errorf("the quarantine path ended at %s; a lease short of terminal holds its credit forever", got.State)
		}
		return nil
	})
}

func TestInvalidTransitionAndStateConflict(t *testing.T) {
	s := newStore(t)
	lease := seedLease(t, s, "conflict")

	err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.TransitionLease(lease.ID, LeaseReserved, LeaseWorkloadRunning)
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("skip-ahead transition = %v; want ErrInvalidTransition", err)
	}

	err = s.Tx(t.Context(), func(tx *Tx) error {
		return tx.TransitionLease(lease.ID, LeaseCleaning, LeaseReleased)
	})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale-from transition = %v; want ErrStateConflict", err)
	}

	err = s.Tx(t.Context(), func(tx *Tx) error {
		return tx.TransitionLease("missing", LeaseReserved, LeaseProvisioning)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lease transition = %v; want ErrNotFound", err)
	}
}

// The lease's runtime name is what a provider observation is correlated
// through, and the observation can arrive after the run it describes has
// ended. So a released lease still has to answer to its name: the attempt
// it ran for is exactly what the late report belongs to.
func TestAttemptOfRuntimeNameSurvivesTheLeaseEnding(t *testing.T) {
	s := newStore(t)
	lease := seedLease(t, s, "runtime")

	inTx(t, s, func(tx *Tx) error { return tx.SetLeaseRuntimeName(lease.ID, "runpool-abc") })
	inTx(t, s, func(tx *Tx) error {
		got, err := tx.AttemptOfRuntimeName("runpool-abc")
		if err != nil {
			return err
		}
		if assignment.AttemptID(got) != lease.AttemptID {
			t.Errorf("runtime name resolved to attempt %q; want %q", got, lease.AttemptID)
		}
		return nil
	})

	for _, step := range [][2]LeaseState{
		{LeaseReserved, LeaseProvisioning},
		{LeaseProvisioning, LeaseFailed},
		{LeaseFailed, LeaseCleaning},
		{LeaseCleaning, LeaseReleased},
	} {
		inTx(t, s, func(tx *Tx) error { return tx.TransitionLease(lease.ID, step[0], step[1]) })
	}

	inTx(t, s, func(tx *Tx) error {
		got, err := tx.AttemptOfRuntimeName("runpool-abc")
		if err != nil {
			t.Errorf("a released lease stopped answering to its runtime name: %v", err)
			return nil
		}
		if assignment.AttemptID(got) != lease.AttemptID {
			t.Errorf("after release the name resolved to attempt %q; want %q", got, lease.AttemptID)
		}
		return nil
	})

	// A second lease that never registered a runtime carries the column
	// default. Without it this assertion asserts nothing: no row exists
	// for the unguarded query to match.
	seedLease(t, s, "runtime-unnamed")
	inTx(t, s, func(tx *Tx) error {
		if _, err := tx.AttemptOfRuntimeName(""); !errors.Is(err, ErrNotFound) {
			t.Errorf("an empty runtime name resolved to %v; the default column value "+
				"must never match", err)
		}
		return nil
	})
}

func TestTxRollsBackOnError(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-rollback", "job-rollback")

	boom := errors.New("boom")
	err := s.Tx(t.Context(), func(tx *Tx) error {
		if _, err := tx.LeaseAttempt(assignment.AttemptID(attempt), assignment.BindingID(binding), "standard"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	inTx(t, s, func(tx *Tx) error {
		leases, err := tx.LeasesInStates(LeaseReserved)
		if err != nil {
			return err
		}
		if len(leases) != 0 {
			t.Errorf("rolled-back lease persisted: %+v", leases)
		}
		// The claim rolled back with it, so the work is servable again.
		ready, err := tx.AllReadyAttempts(assignment.BindingID(binding))
		if err != nil {
			return err
		}
		if len(ready) != 1 {
			t.Errorf("ready attempts after rollback = %d; want the claim undone", len(ready))
		}
		return nil
	})
}

// TestResourceIntentLifecycleAndReleaseRule walks one intent through
// its whole life — planned before any effect, creating in the ambiguous
// window, present once confirmed, cleanup_pending and deleting on the
// way out, absent as the row's deletion — and proves the RESTRICT
// foreign key: a lease row cannot vanish while an intent still names
// anything.
func TestResourceIntentLifecycleAndReleaseRule(t *testing.T) {
	s := newStore(t)
	lease := seedLease(t, s, "intents")

	var netIntent, dindIntent assignment.ResourceIntentID
	inTx(t, s, func(tx *Tx) error {
		var err error
		if netIntent, err = tx.PlanResource(lease.ID, ResourceNetwork, ResourceRoleCapsuleNetwork, "runpool-net-x"); err != nil {
			return err
		}
		dindIntent, err = tx.PlanResource(lease.ID, ResourceContainer, ResourceRoleCapsule, "runpool-dind-x")
		return err
	})

	// The RESTRICT foreign key is the release rule: a lease row cannot
	// vanish while intents still name anything — even unconfirmed ones,
	// whose objects may exist.
	err := s.Tx(t.Context(), func(tx *Tx) error {
		_, err := tx.tx.Exec(`DELETE FROM capsule_leases WHERE id = ?`, lease.ID)
		return err
	})
	if err == nil {
		t.Fatal("deleting a lease with live intents succeeded; want FK failure")
	}

	inTx(t, s, func(tx *Tx) error {
		// The network confirms; the dind create crashes mid-window and
		// stays creating, reachable only by its deterministic name.
		if err := tx.MarkResourceCreating(netIntent); err != nil {
			return err
		}
		if err := tx.MarkResourcePresent(netIntent, "net-abc"); err != nil {
			return err
		}
		return tx.MarkResourceCreating(dindIntent)
	})

	inTx(t, s, func(tx *Tx) error {
		intents, err := tx.Resources(lease.ID)
		if err != nil {
			return err
		}
		if len(intents) != 2 {
			t.Fatalf("intents = %d; want 2", len(intents))
		}
		for _, in := range intents {
			switch in.ID {
			case netIntent:
				if in.State != "present" || in.Handle() != "net-abc" {
					t.Errorf("confirmed intent = %s/%s; want present, addressed by id", in.State, in.Handle())
				}
			case dindIntent:
				if in.State != "creating" || in.Handle() != "runpool-dind-x" {
					t.Errorf("unconfirmed intent = %s/%s; want creating, addressed by name", in.State, in.Handle())
				}
			}
		}
		return nil
	})

	inTx(t, s, func(tx *Tx) error {
		// Removal queues every intent — confirmed or not: a creating
		// intent's object may exist, and only the delete path proves
		// otherwise.
		if err := tx.MarkResourceCleanup(lease.ID); err != nil {
			return err
		}
		intents, err := tx.Resources(lease.ID)
		if err != nil {
			return err
		}
		for _, in := range intents {
			if in.State != "cleanup_pending" {
				t.Errorf("intent %d = %s; want cleanup_pending", in.ID, in.State)
			}
			if err := tx.MarkResourceDeleting(in.ID); err != nil {
				return err
			}
			if err := tx.ForgetResource(in.ID); err != nil {
				return err
			}
		}
		if err := tx.ForgetResource(999); !errors.Is(err, ErrNotFound) {
			t.Errorf("forgetting an unknown intent = %v; want ErrNotFound", err)
		}
		return nil
	})

	// With every intent absent and the attempt resolved, the lease is
	// purgeable.
	inTx(t, s, func(tx *Tx) error {
		if err := tx.Settle(lease.AttemptID, AttemptLeased, assignment.ResolutionCompletedObserved); err != nil {
			return err
		}
		return tx.PurgeLease(lease.ID)
	})
}

// TestPendingRemovalsHonourBackoff: a failed removal is booked with a
// not-before, and the periodic reconciler's working set excludes it
// until then — per resource, so one wedged object does not stall the
// rest.
func TestPendingRemovalsHonourBackoff(t *testing.T) {
	s := newStore(t)
	lease := seedLease(t, s, "backoff")

	var wedged, healthy assignment.ResourceIntentID
	inTx(t, s, func(tx *Tx) error {
		var err error
		if wedged, err = tx.PlanResource(lease.ID, ResourceVolume, ResourceRoleDindData, "runpool-work-x"); err != nil {
			return err
		}
		if healthy, err = tx.PlanResource(lease.ID, ResourceVolume, ResourceRoleDindData, "runpool-run-x"); err != nil {
			return err
		}
		return tx.MarkResourceCleanup(lease.ID)
	})

	now := time.Now()
	inTx(t, s, func(tx *Tx) error {
		return tx.RecordResourceError(wedged, errors.New("daemon busy"), now.Add(time.Hour))
	})

	inTx(t, s, func(tx *Tx) error {
		due, err := tx.pendingRemovals(now, 10)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != healthy {
			t.Fatalf("due removals = %+v; want only the healthy intent", due)
		}
		later, err := tx.pendingRemovals(now.Add(2*time.Hour), 10)
		if err != nil {
			return err
		}
		if len(later) != 2 {
			t.Errorf("after the backoff both must be due; got %d", len(later))
		}
		for _, in := range later {
			if in.ID == wedged && (in.Retries != 1 || in.LastError == "") {
				t.Errorf("wedged intent bookkeeping = retries %d, err %q", in.Retries, in.LastError)
			}
		}
		return nil
	})
}

func TestBackupIsRestorable(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	seedLease(t, s, "backup")

	backupDir := t.TempDir()
	if err := s.Backup(t.Context(), filepath.Join(backupDir, DatabaseFile)); err != nil {
		t.Fatal(err)
	}

	restored := openStore(t, backupDir)
	if restored.InstanceID() != s.InstanceID() {
		t.Error("backup lost the instance identity")
	}
	inTx(t, restored, func(tx *Tx) error {
		leases, err := tx.LeasesInStates(LeaseReserved)
		if err != nil {
			return err
		}
		if len(leases) != 1 {
			t.Errorf("restored leases = %d; want 1", len(leases))
		}
		return nil
	})
}

// A schema that already holds data is backed up before the first pending
// migration touches it — the copy an operator restores if the upgrade is
// what broke the database.
func TestPendingMigrationBacksUpFirst(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	seedLease(t, s, "migrate")

	migrations, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	synthetic := append(migrations, migration{
		version: len(migrations) + 1,
		name:    "synthetic",
		up:      `CREATE TABLE synthetic_two (id INTEGER PRIMARY KEY);`,
	})
	if err := s.applyMigrations(synthetic); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(); v != len(synthetic) {
		t.Fatalf("schema version = %d; want %d", v, len(synthetic))
	}
	backup := filepath.Join(dir, fmt.Sprintf("pre-migration-v%d.db", len(migrations)))

	// Opened raw, not through Open: Open migrates, and a backup this build
	// re-migrated on inspection could not show what the copy held. What
	// makes the file a rollback path is its contents -- the version the
	// schema had before the upgrade, the rows that were live, and nothing
	// the upgrade added. A file that merely exists proves none of that: an
	// empty file exists, and a copy taken after the loop exists too.
	db, err := sql.Open("sqlite", DSN(backup))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("the backup holds schema version %d; want the pre-migration %d -- a copy taken "+
			"after the upgrade is not a rollback path", version, len(migrations))
	}
	var leases int
	if err := db.QueryRow(`SELECT count(*) FROM capsule_leases`).Scan(&leases); err != nil {
		t.Fatalf("the backup could not answer for its rows: %v", err)
	}
	if leases != 1 {
		t.Errorf("the backup holds %d leases; want the 1 that was live -- restoring it would lose data", leases)
	}
	var added int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'synthetic_two'`).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Error("the backup holds the table the pending migration created; the copy was taken after the upgrade")
	}
}

// TestLeaseStateListsCoverTheMachine. LeaseStates is what reporting,
// the reconciler's working set and the resource sweep all enumerate, and
// it is written by hand — so a state the machine can reach but the list
// omits is invisible to every one of them, and a lease in it is treated
// as though it does not exist. The machine is the anchor: transitions
// names every state, as an origin or as a destination.
func TestLeaseStateListsCoverTheMachine(t *testing.T) {
	inMachine := map[LeaseState]bool{}
	for from, tos := range transitions {
		inMachine[from] = true
		for _, to := range tos {
			inMachine[to] = true
		}
	}
	listed := map[LeaseState]bool{}
	for _, s := range LeaseStates() {
		listed[s] = true
	}
	for s := range inMachine {
		if !listed[s] {
			t.Errorf("the machine reaches %s but LeaseStates omits it; every report and "+
				"sweep enumerates that list, so leases in that state are invisible to all of them", s)
		}
	}
	for s := range listed {
		if !inMachine[s] {
			t.Errorf("LeaseStates names %s, which no transition reaches", s)
		}
	}

	// Live is the same set without the terminal states. A sweep trusts it
	// to be exhaustive, so both halves are checked: nothing terminal in
	// it, and nothing live left out of it.
	live := map[LeaseState]bool{}
	for _, s := range LiveLeaseStates() {
		if s.Terminal() {
			t.Errorf("LiveLeaseStates carries the terminal state %s", s)
		}
		live[s] = true
	}
	for _, s := range LeaseStates() {
		if !s.Terminal() && !live[s] {
			t.Errorf("%s is live but missing from LiveLeaseStates; a snapshot without it makes "+
				"cleanup delete the resources of a capsule that is running", s)
		}
	}
}

func assertVocabularySnapshot[T comparable](t *testing.T, values func() []T) {
	t.Helper()
	first := values()
	if len(first) == 0 {
		t.Fatal("vocabulary is empty")
	}
	want := first[0]
	var changed T
	first[0] = changed
	if got := values()[0]; got != want {
		t.Errorf("caller mutation changed the vocabulary: got %v, want %v", got, want)
	}
}

func TestVocabularySnapshotsCannotMutateTheStoreDomain(t *testing.T) {
	assertVocabularySnapshot(t, LeaseStates)
	assertVocabularySnapshot(t, LiveLeaseStates)
	assertVocabularySnapshot(t, ResourceKinds)
	assertVocabularySnapshot(t, ResourceRoles)
	assertVocabularySnapshot(t, ResourceStates)
	assertVocabularySnapshot(t, AttemptStates)
	assertVocabularySnapshot(t, EvidenceStates)
	assertVocabularySnapshot(t, ReviewReasons)
	assertVocabularySnapshot(t, EventKinds)
}
