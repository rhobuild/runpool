package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"
)

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return openStore(t, t.TempDir())
}

func inTx(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	if err := s.Tx(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func seedBinding(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	inTx(t, s, func(tx *Tx) error {
		var err error
		id, err = tx.EnsureBinding("default", "github_actions",
			"v1|repository|https://github.com/acme/app||runpool-standard")
		return err
	})
	return id
}

// seedAttempt records one delivery carrying one workload and returns the
// ready attempt it produced — the starting point for anything that needs
// work to lease.
func seedAttempt(t *testing.T, s *Store, binding int64, deliveryKey, workloadKey string) string {
	t.Helper()
	var id string
	inTx(t, s, func(tx *Tx) error {
		if _, err := tx.RecordDelivery(binding, deliveryKey, fingerprint(deliveryKey),
			[]WorkloadRow{{SourceWorkloadKey: workloadKey, TenantKey: "acme", ProjectKey: "app"}}); err != nil {
			return err
		}
		attempt, err := tx.q.GetOpenAttemptByWorkload(tx.ctx, sqlitedb.GetOpenAttemptByWorkloadParams{
			BindingID: binding, SourceWorkloadKey: workloadKey,
		})
		id = attempt.ID
		return err
	})
	return id
}

func fingerprint(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// A redelivery repeats the delivery byte for byte, so it lands on the
// same delivery row and creates no second attempt. The same natural key
// with different content is contract drift: nothing is written, nothing
// may be acknowledged, and the caller stops the binding.
func TestRedeliveryIsIdempotentAndDriftFailsClosed(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	workloads := []WorkloadRow{{SourceWorkloadKey: "job-1", TenantKey: "acme", ProjectKey: "app"}}

	var first sqlitedb.BrokerDelivery
	inTx(t, s, func(tx *Tx) error {
		var err error
		first, err = tx.RecordDelivery(binding, "msg-7", fingerprint("payload-a"), workloads)
		return err
	})

	// Exact redelivery: same row back, still exactly one attempt.
	inTx(t, s, func(tx *Tx) error {
		again, err := tx.RecordDelivery(binding, "msg-7", fingerprint("payload-a"), workloads)
		if err != nil {
			return err
		}
		if again.ID != first.ID {
			t.Errorf("redelivery produced delivery %d; want the original %d", again.ID, first.ID)
		}
		n, err := tx.q.CountOpenAttempts(tx.ctx, binding)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("open attempts after redelivery = %d; want 1", n)
		}
		return nil
	})

	// Drift: same key, different payload.
	err := s.Tx(t.Context(), func(tx *Tx) error {
		_, err := tx.RecordDelivery(binding, "msg-7", fingerprint("payload-B"), workloads)
		return err
	})
	if !errors.Is(err, ErrContractDrift) {
		t.Fatalf("drifted redelivery returned %v; want ErrContractDrift", err)
	}
}

// A reassignment arrives as a new delivery for a workload whose previous
// attempt is settled: representable, and served as a fresh attempt. The
// same arrival while the predecessor is still open is refused until the
// caller supersedes it — in the same transaction — because two live
// attempts for one workload is how a job runs twice.
func TestReassignmentNeedsThePredecessorResolved(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	workloads := []WorkloadRow{{SourceWorkloadKey: "job-r", TenantKey: "acme", ProjectKey: "app"}}

	inTx(t, s, func(tx *Tx) error {
		_, err := tx.RecordDelivery(binding, "msg-1", fingerprint("first"), workloads)
		return err
	})

	// While the first attempt is open, the new delivery is refused.
	err := s.Tx(t.Context(), func(tx *Tx) error {
		_, err := tx.RecordDelivery(binding, "msg-2", fingerprint("second"), workloads)
		return err
	})
	if !errors.Is(err, ErrOpenAttemptExists) {
		t.Fatalf("second open attempt returned %v; want ErrOpenAttemptExists", err)
	}

	// Supersede-then-record commits together: the partial index admits
	// the new attempt in the same transaction that closes the old one.
	inTx(t, s, func(tx *Tx) error {
		if err := tx.SupersedeOpenAttempt(binding, "job-r", "reassigned_by_provider"); err != nil {
			return err
		}
		_, err := tx.RecordDelivery(binding, "msg-2", fingerprint("second"), workloads)
		return err
	})

	inTx(t, s, func(tx *Tx) error {
		ready, err := tx.ReadyAttempts(binding)
		if err != nil {
			return err
		}
		if len(ready) != 1 || ready[0].SourceWorkloadKey != "job-r" {
			t.Errorf("ready attempts = %+v; want exactly the reassigned workload", ready)
		}
		n, err := tx.q.CountOpenAttempts(tx.ctx, binding)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("open attempts = %d; want 1 — the superseded one must not count", n)
		}
		return nil
	})
}

// The one-open-attempt rule is the database's, not a map's: two
// transactions racing to open an attempt for the same workload cannot
// both win, whatever the interleaving.
func TestOneOpenAttemptUnderContention(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)

	results := make(chan error, 2)
	for i := range 2 {
		key := "msg-c" + string(rune('a'+i))
		go func() {
			results <- s.Tx(context.Background(), func(tx *Tx) error {
				_, err := tx.RecordDelivery(binding, key, fingerprint(key),
					[]WorkloadRow{{SourceWorkloadKey: "job-contended", TenantKey: "acme", ProjectKey: "app"}})
				return err
			})
		}()
	}
	var failures, wins int
	for range 2 {
		if err := <-results; err != nil {
			if !errors.Is(err, ErrOpenAttemptExists) {
				t.Fatalf("loser failed with %v; want ErrOpenAttemptExists", err)
			}
			failures++
		} else {
			wins++
		}
	}
	if wins != 1 || failures != 1 {
		t.Fatalf("wins=%d failures=%d; exactly one transaction may open the attempt", wins, failures)
	}
}

// Attempt events are idempotent per key: recording the same observation
// twice is one event, so a redelivered message cannot double history.
func TestAttemptEventsAreIdempotent(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-e", "job-e")

	inTx(t, s, func(tx *Tx) error {
		for range 2 {
			if err := tx.RecordEvent(attempt, "lease_attached:l1", "lease_attached"); err != nil {
				return err
			}
		}
		events, err := tx.Events(attempt)
		if err != nil {
			return err
		}
		// attempt_created plus exactly one lease_attached.
		if len(events) != 2 {
			t.Errorf("events = %d; want 2 — the repeated observation must be one event", len(events))
		}
		return nil
	})
}

// The ack state machine survives ambiguity. A timeout after a remote
// success leaves the delivery uncertain; the retry path marks it in
// flight again and converges to confirmed, and a confirmed delivery
// refuses to go back in flight — the caller skips the network call
// entirely.
func TestAckStateMachineConvergesAfterUncertainty(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)

	var deliveryID int64
	inTx(t, s, func(tx *Tx) error {
		d, err := tx.RecordDelivery(binding, "msg-ack", fingerprint("ack"),
			[]WorkloadRow{{SourceWorkloadKey: "job-ack", TenantKey: "acme", ProjectKey: "app"}})
		deliveryID = d.ID
		return err
	})

	// First cycle: in flight, then the call times out ambiguously.
	inTx(t, s, func(tx *Tx) error {
		proceed, err := tx.AckRequested(deliveryID)
		if err != nil {
			return err
		}
		if !proceed {
			t.Error("a pending delivery refused to go in flight")
		}
		return tx.AckUncertain(deliveryID)
	})

	// Retry cycle: uncertain goes back in flight and confirms.
	inTx(t, s, func(tx *Tx) error {
		proceed, err := tx.AckRequested(deliveryID)
		if err != nil {
			return err
		}
		if !proceed {
			t.Error("an uncertain delivery refused the retry; it would never converge")
		}
		return tx.AckConfirmed(deliveryID)
	})

	// Confirmed is terminal: no further network call is warranted, and
	// re-confirming is idempotent.
	inTx(t, s, func(tx *Tx) error {
		proceed, err := tx.AckRequested(deliveryID)
		if err != nil {
			return err
		}
		if proceed {
			t.Error("a confirmed delivery went back in flight; the caller would re-ack forever")
		}
		return tx.AckConfirmed(deliveryID)
	})
}

// Leasing is the compare-and-swap that decides who serves a workload.
// The loser writes nothing at all: an orphan lease row pointing at an
// attempt somebody else is running is a capacity leak and a second
// runtime for one job.
func TestLeaseAttemptClaimsExactlyOnce(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-claim", "job-claim")

	var lease Lease
	inTx(t, s, func(tx *Tx) error {
		var err error
		lease, err = tx.LeaseAttempt(attempt, binding, "standard")
		return err
	})
	if lease.State != LeaseReserved {
		t.Errorf("new lease state = %s; want reserved", lease.State)
	}
	if lease.AttemptID != attempt {
		t.Errorf("lease serves attempt %q; want %q", lease.AttemptID, attempt)
	}

	err := s.Tx(t.Context(), func(tx *Tx) error {
		_, err := tx.LeaseAttempt(attempt, binding, "standard")
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second lease of the same attempt = %v; want ErrConflict", err)
	}
	inTx(t, s, func(tx *Tx) error {
		leases, err := tx.LeasesInStates(AllLeaseStates...)
		if err != nil {
			return err
		}
		if len(leases) != 1 {
			t.Errorf("leases = %d; the losing claim must leave no row behind", len(leases))
		}
		return nil
	})
}

// Evidence belongs to the attempt and is strictly monotonic. Re-observing
// a fact is idempotent; a write that would move backwards is refused
// rather than silently dropped, because an observation that was made
// cannot be unmade by a slower writer.
func TestEvidenceIsMonotonicAndOwnedByTheAttempt(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-ev", "job-ev")

	inTx(t, s, func(tx *Tx) error {
		for _, e := range []Evidence{EvidenceRuntimePrepared, EvidenceStartAuthorized, EvidenceRunningObserved} {
			if err := tx.RecordEvidence(attempt, e); err != nil {
				return err
			}
		}
		// Re-observing the same fact is not a fault.
		return tx.RecordEvidence(attempt, EvidenceRunningObserved)
	})

	err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.RecordEvidence(attempt, EvidenceRuntimePrepared)
	})
	if !errors.Is(err, ErrObservationConflict) {
		t.Fatalf("backwards evidence = %v; want ErrObservationConflict", err)
	}

	err = s.Tx(t.Context(), func(tx *Tx) error {
		return tx.RecordEvidence(attempt, Evidence("probably_fine"))
	})
	if !errors.Is(err, ErrInvalidExecutionObservation) {
		t.Fatalf("unknown observation = %v; want ErrInvalidExecutionObservation", err)
	}

	inTx(t, s, func(tx *Tx) error {
		got, err := tx.Get(attempt)
		if err != nil {
			return err
		}
		if got.Evidence != EvidenceRunningObserved {
			t.Errorf("evidence = %s; want running_observed", got.Evidence)
		}
		if got.Evidence.Retriable() {
			t.Error("observed-running work must not be retriable")
		}
		return nil
	})
}

// A lease may not be purged while the attempt it serves is unresolved:
// deleting the runtime record of live work leaves an attempt nothing can
// finish or explain. Once the attempt is settled the lease is purgeable,
// and purging takes the link with it — which is why settlement no longer
// has to clear anything.
func TestPurgeLeaseRefusesWhileItsAttemptIsUnresolved(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-fk", "job-fk")

	var leaseID string
	inTx(t, s, func(tx *Tx) error {
		lease, err := tx.LeaseAttempt(attempt, binding, "standard")
		leaseID = lease.ID
		return err
	})

	if err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.PurgeLease(leaseID)
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("purging a lease whose attempt is live = %v; want ErrConflict", err)
	}

	inTx(t, s, func(tx *Tx) error {
		if err := tx.Settle(attempt, "leased", "completed_observed"); err != nil {
			return err
		}
		return tx.PurgeLease(leaseID)
	})

	if err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.PurgeLease(leaseID)
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("purging an absent lease = %v; want ErrNotFound", err)
	}
}

// StrandedAttempts finds the crash window between releasing a lease and
// disposing of its attempt. It reads through the lease's own link, so a
// purged lease cannot leave a phantom row behind.
func TestStrandedAttemptsSeeReleasedLeases(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-strand", "job-strand")

	inTx(t, s, func(tx *Tx) error {
		lease, err := tx.LeaseAttempt(attempt, binding, "standard")
		if err != nil {
			return err
		}
		for _, step := range [][2]LeaseState{
			{LeaseReserved, LeaseProvisioning},
			{LeaseProvisioning, LeaseFailed},
			{LeaseFailed, LeaseCleaning},
			{LeaseCleaning, LeaseReleased},
		} {
			if err := tx.TransitionLease(lease.ID, step[0], step[1]); err != nil {
				return err
			}
		}
		return nil
	})

	inTx(t, s, func(tx *Tx) error {
		stranded, err := tx.StrandedAttempts()
		if err != nil {
			return err
		}
		if len(stranded) != 1 || stranded[0].ID != attempt {
			t.Fatalf("stranded = %+v; want the attempt of the released lease", stranded)
		}
		return nil
	})
}

// The committed schema snapshot must be exactly what the migrations
// produce: sqlc compiled against anything else generates code for a
// database that does not exist.
func TestSchemaSnapshotMatchesMigrations(t *testing.T) {
	generated, err := SchemaProjection(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("schema/current.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != generated {
		t.Error("internal/store/schema/current.sql is stale; regenerate with: go run ./internal/store/schema/gen")
	}
}

// sqlc's sqlite lexer loses the comment boundary on multibyte
// characters and reports phantom parse errors statements later. The
// query files stay ASCII so that failure mode cannot return.
func TestQueryFilesAreASCII(t *testing.T) {
	entries, err := os.ReadDir("query")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, err := os.ReadFile("query/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range body {
			if b > 0x7f {
				line := 1 + strings.Count(string(body[:i]), "\n")
				t.Errorf("%s:%d: non-ASCII byte 0x%x; sqlc v1.31.1's sqlite lexer mis-parses it", e.Name(), line, b)
				break
			}
		}
	}
}

// A crash between marking the acknowledgement in flight and recording its
// outcome leaves the delivery in `requested`. The broker was never told, so
// it redelivers the same message forever — and if `requested` refused to go
// back in flight, the retry became a no-op and the binding stopped serving
// everything queued behind it. That state is the one a retry exists for.
func TestAckRequestedIsRetriedAfterACrashInFlight(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)

	var deliveryID int64
	inTx(t, s, func(tx *Tx) error {
		d, err := tx.RecordDelivery(binding, "msg-wedge", fingerprint("wedge"),
			[]WorkloadRow{{SourceWorkloadKey: "job-wedge", TenantKey: "acme", ProjectKey: "app"}})
		deliveryID = d.ID
		return err
	})

	// In flight, then the process dies: no outcome is ever recorded.
	inTx(t, s, func(tx *Tx) error {
		proceed, err := tx.AckRequested(deliveryID)
		if err != nil {
			return err
		}
		if !proceed {
			t.Fatal("a pending delivery refused to go in flight")
		}
		return nil
	})

	// The successor must be able to retry it.
	inTx(t, s, func(tx *Tx) error {
		proceed, err := tx.AckRequested(deliveryID)
		if err != nil {
			return err
		}
		if !proceed {
			t.Error("a delivery stranded in flight refused the retry; its binding would wedge forever")
		}
		return tx.AckConfirmed(deliveryID)
	})
}

// TestPurgeEverythingClearsCacheLanes: uninstall deletes the lane volumes,
// so leaving their rows behind on a retained state volume strands them.
// Every surviving row keeps a leased_by naming a lease that no longer
// exists, and no supported path can reclaim one — reuse needs leased_by
// IS NULL, GC only considers unleased lanes, and DeleteCacheLane refuses a
// leased row. A reinstall would find the project already at its lane
// ceiling and run every job for it uncached, permanently.
func TestPurgeEverythingClearsCacheLanes(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-purge", "job-purge")

	var projectID string
	inTx(t, s, func(tx *Tx) error {
		var err error
		if projectID, err = tx.EnsureCacheProject("acme/app"); err != nil {
			return err
		}
		lease, err := tx.LeaseAttempt(attempt, binding, "standard")
		if err != nil {
			return err
		}
		_, err = tx.LeaseCacheLane(projectID, "default", lease.ID, 2)
		return err
	})

	inTx(t, s, func(tx *Tx) error { return tx.PurgeEverything() })

	// The proof that nothing is stranded: a fresh lease can take a lane
	// with a ceiling of one, which a surviving row would have consumed.
	// Seeded outside the transaction below: the pool is one connection, so
	// opening a second transaction inside the first deadlocks.
	binding = seedBinding(t, s)
	fresh := seedAttempt(t, s, binding, "msg-after", "job-after")
	inTx(t, s, func(tx *Tx) error {
		lease, err := tx.LeaseAttempt(fresh, binding, "standard")
		if err != nil {
			return err
		}
		id, err := tx.EnsureCacheProject("acme/app")
		if err != nil {
			return err
		}
		if _, err := tx.LeaseCacheLane(id, "default", lease.ID, 1); err != nil {
			t.Errorf("a lane survived uninstall and consumed the ceiling: %v", err)
		}
		return nil
	})
}

// TestSnapshotBoundsHistoryButNeverLiveWork is the asymmetry that decides
// this design. Dropping a released lease costs a line of history;
// dropping a live one makes the report call that job's containers orphans
// and makes cleanup delete the resources of a capsule that is running.
// So released is bounded and live is not, and the count stays the store's
// total rather than what the slice happens to carry.
func TestSnapshotBoundsHistoryButNeverLiveWork(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)

	const released = ReportedReleasedLeases + 20
	// Finish times an hour apart. Seeded in a loop they would all land in
	// the same second, and a set selected by time would then be decided by
	// the random id tiebreak — which is how an order assertion passes
	// whichever way the query is written.
	base := time.Now().Add(-released * time.Hour)
	finishOrder := make([]string, released)
	for i := range released {
		id, _ := releasedLease(t, s, binding, fmt.Sprintf("done-%03d", i))
		backdateLease(t, s, id, base, base.Add(time.Duration(i)*time.Hour))
		finishOrder[i] = id
	}

	// Three live leases, each in a different state.
	live := map[string]bool{}
	for i, state := range []LeaseState{LeaseReserved, LeaseProvisioning, LeaseRuntimeRegistered} {
		key := fmt.Sprintf("live-%d", i)
		attempt := seedAttempt(t, s, binding, "msg-"+key, "job-"+key)
		inTx(t, s, func(tx *Tx) error {
			lease, err := tx.LeaseAttempt(attempt, binding, "standard")
			if err != nil {
				return err
			}
			live[lease.ID] = true
			if state == LeaseReserved {
				return nil
			}
			if err := tx.TransitionLease(lease.ID, LeaseReserved, LeaseProvisioning); err != nil {
				return err
			}
			if state == LeaseProvisioning {
				return nil
			}
			return tx.TransitionLease(lease.ID, LeaseProvisioning, LeaseRuntimeRegistered)
		})
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	var gotLive, gotReleased int
	seen := map[string]bool{}
	for _, l := range snap.Leases {
		seen[l.ID] = true
		if l.State.Terminal() {
			gotReleased++
		} else {
			gotLive++
		}
	}

	// Every live lease, without exception.
	for id := range live {
		if !seen[id] {
			t.Errorf("live lease %s is missing from the snapshot; its containers would be "+
				"reported as orphans and cleanup would delete a running capsule's resources", id)
		}
	}
	if gotLive != len(live) {
		t.Errorf("live leases in snapshot = %d; want %d", gotLive, len(live))
	}

	// Released history is bounded, and the count still tells the truth.
	if gotReleased != ReportedReleasedLeases {
		t.Errorf("released leases in snapshot = %d; want the %d most recent", gotReleased, ReportedReleasedLeases)
	}
	if snap.ReleasedTotal != released {
		t.Errorf("ReleasedTotal = %d; want every released lease, %d", snap.ReleasedTotal, released)
	}

	// The ones carried are the ones that finished last, emitted oldest-first
	// — selection order and emission order are opposite, so both halves are
	// named here rather than left to a monotonicity check that a reversed
	// query would also satisfy.
	var carried []string
	for _, l := range snap.Leases {
		if l.State.Terminal() {
			carried = append(carried, l.ID)
		}
	}
	want := finishOrder[released-ReportedReleasedLeases:]
	if !slices.Equal(carried, want) {
		t.Errorf("released leases carried = %v;\nwant the %d that finished last, oldest-first: %v",
			carried, ReportedReleasedLeases, want)
	}
}

// backdateLease ages a lease so a retention window can be tested without
// waiting for one. Start and finish are separate arguments because the two
// are the whole question: a lease that ran for months finished once, and
// only the second of those times says whether its record is still wanted.
func backdateLease(t *testing.T, s *Store, leaseID string, started, finished time.Time) {
	t.Helper()
	inTx(t, s, func(tx *Tx) error {
		_, err := tx.tx.Exec(
			`UPDATE capsule_leases SET created_at = ?, updated_at = ? WHERE id = ?`,
			started.Unix(), finished.Unix(), leaseID)
		return err
	})
}

// releasedLease drives one attempt to a released lease and returns both ids.
func releasedLease(t *testing.T, s *Store, binding int64, key string) (leaseID, attemptID string) {
	t.Helper()
	attemptID = seedAttempt(t, s, binding, "msg-"+key, "job-"+key)
	inTx(t, s, func(tx *Tx) error {
		lease, err := tx.LeaseAttempt(attemptID, binding, "standard")
		if err != nil {
			return err
		}
		leaseID = lease.ID
		for _, step := range [][2]LeaseState{
			{LeaseReserved, LeaseProvisioning},
			{LeaseProvisioning, LeaseRuntimeRegistered},
			{LeaseRuntimeRegistered, LeaseDraining},
			{LeaseDraining, LeaseCleaning},
			{LeaseCleaning, LeaseReleased},
		} {
			if err := tx.TransitionLease(leaseID, step[0], step[1]); err != nil {
				return err
			}
		}
		return nil
	})
	return leaseID, attemptID
}

// TestRetentionMeasuresFromTheFinish. The window is how long a finished
// lease is remembered, so it has to be measured from when the lease
// finished. Measuring from when it started forgets exactly the records
// worth keeping: a lease that wedged in quarantine for months and was
// resolved a minute ago is the one an operator is looking at, and its
// start is far enough back that any window puts it out of reach. It would
// be deleted within one reconcile tick of finishing.
func TestRetentionMeasuresFromTheFinish(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	longAgo := time.Now().Add(-95 * 24 * time.Hour)
	cutoff := time.Now().Add(-90 * 24 * time.Hour)

	// Started 95 days ago and finished 95 days ago: ordinary old work.
	settled, settledAttempt := releasedLease(t, s, binding, "settled")
	backdateLease(t, s, settled, longAgo, longAgo)

	// Started 95 days ago, wedged, resolved a minute ago.
	resolved, resolvedAttempt := releasedLease(t, s, binding, "resolved")
	backdateLease(t, s, resolved, longAgo, time.Now().Add(-time.Minute))

	inTx(t, s, func(tx *Tx) error {
		if err := tx.Settle(settledAttempt, "leased", "completed_observed"); err != nil {
			return err
		}
		return tx.Settle(resolvedAttempt, "leased", "completed_observed")
	})

	var removed int
	inTx(t, s, func(tx *Tx) error {
		var err error
		removed, err = tx.PruneLeaseHistory(cutoff, 100)
		return err
	})
	if removed != 1 {
		t.Fatalf("pruned %d leases; only the one that finished outside the window qualifies", removed)
	}
	inTx(t, s, func(tx *Tx) error {
		if _, err := tx.LeaseByID(resolved); err != nil {
			t.Errorf("a lease that finished a minute ago was forgotten: %v — "+
				"the window is measured from the finish, not from the start", err)
		}
		if _, err := tx.LeaseByID(settled); !errors.Is(err, ErrNotFound) {
			t.Errorf("a lease that finished 95 days ago survived a 90 day window: %v", err)
		}
		return nil
	})

	// And reporting ranks by the same clock: "recent" means recently
	// finished, or the record just resolved is the one the bound hides.
	inTx(t, s, func(tx *Tx) error {
		recent, _, err := tx.RecentReleasedLeases(1)
		if err != nil {
			return err
		}
		if len(recent) != 1 || recent[0].ID != resolved {
			t.Errorf("the single most recent released lease = %+v; want %s, which finished last",
				recent, resolved)
		}
		return nil
	})
}

// TestPruneLeaseHistoryHonoursBothGuards. Each guard stops a different
// loss, and neither is optional.
//
// An attempt left open by a crash is found by joining through its
// released lease — StrandedAttempts is the only thing that looks for it —
// so deleting that lease would make the attempt unreachable forever.
// And a lease that still owns a resource intent is a real leak: the
// foreign key would fail the delete, and the row must stay visible rather
// than be forced away.
func TestPruneLeaseHistoryHonoursBothGuards(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	old := time.Now().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	// Ordinary finished work, old enough to forget.
	prunable, prunableAttempt := releasedLease(t, s, binding, "old")
	backdateLease(t, s, prunable, old, old)
	inTx(t, s, func(tx *Tx) error {
		return tx.Settle(prunableAttempt, "leased", "completed_observed")
	})

	// Finished, but recent: outside the window.
	recent, recentAttempt := releasedLease(t, s, binding, "recent")
	inTx(t, s, func(tx *Tx) error {
		return tx.Settle(recentAttempt, "leased", "completed_observed")
	})

	// Old and released, but its attempt is still open — the crash window.
	stranded, _ := releasedLease(t, s, binding, "stranded")
	backdateLease(t, s, stranded, old, old)

	// Old and released, but cleanup never finished: an intent survives.
	wedged, wedgedAttempt := releasedLease(t, s, binding, "wedged")
	backdateLease(t, s, wedged, old, old)
	inTx(t, s, func(tx *Tx) error {
		if err := tx.Settle(wedgedAttempt, "leased", "completed_observed"); err != nil {
			return err
		}
		_, err := tx.PlanResource(wedged, ResourceContainer, "runner", "runpool-wedged")
		return err
	})

	var removed int
	inTx(t, s, func(tx *Tx) error {
		count, err := tx.CountPrunableLeases(cutoff, 100)
		if err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("dry run counted %d prunable leases; only the settled old one qualifies", count)
		}
		removed, err = tx.PruneLeaseHistory(cutoff, 100)
		return err
	})
	if removed != 1 {
		t.Fatalf("pruned %d leases; want exactly the settled old one", removed)
	}

	inTx(t, s, func(tx *Tx) error {
		if _, err := tx.LeaseByID(prunable); !errors.Is(err, ErrNotFound) {
			t.Errorf("the settled old lease survived the prune: %v", err)
		}
		if _, err := tx.LeaseByID(recent); err != nil {
			t.Errorf("a lease inside the retention window was pruned: %v", err)
		}
		if _, err := tx.LeaseByID(stranded); err != nil {
			t.Errorf("a lease whose attempt is still open was pruned: %v — "+
				"that attempt is now unreachable by every working set", err)
		}
		if _, err := tx.LeaseByID(wedged); err != nil {
			t.Errorf("a lease still owning a resource intent was pruned: %v — "+
				"the leak it represents must stay visible", err)
		}
		return nil
	})

	// The record of what the work did is untouched: the attempt outlives
	// its runtime plumbing, which is the whole reason this is safe.
	inTx(t, s, func(tx *Tx) error {
		attempt, err := tx.Get(prunableAttempt)
		if err != nil {
			t.Fatalf("the pruned lease's attempt disappeared with it: %v", err)
		}
		if attempt.State != "settled" {
			t.Errorf("attempt state = %q; want the disposition preserved", attempt.State)
		}
		events, err := tx.Events(prunableAttempt)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			t.Error("the pruned lease took its attempt's history with it")
		}
		return nil
	})
}

// backdateAttempt ages an attempt's arrival without waiting for it.
// received_at is what the ready queue is ordered by, so moving it is the
// whole of what growing old means for an attempt.
func backdateAttempt(t *testing.T, s *Store, attemptID string, arrived time.Time) {
	t.Helper()
	inTx(t, s, func(tx *Tx) error {
		_, err := tx.tx.Exec(
			`UPDATE assignment_attempts SET received_at = ? WHERE id = ?`,
			arrived.Unix(), attemptID)
		return err
	})
}

// TestReadyAttemptsAreServedOldestFirst pins the property that decides
// whether a ready attempt can be stranded.
//
// The queue is drained in age order, so the oldest ready attempt is
// always the next one leased. An attempt can therefore only grow old in
// ready if the binding leased nothing at all in that time - admission
// closed, or every lane busy - and in both of those the attempt is
// waiting for this instance, not forgotten by the provider. Without this
// ordering, an old attempt could sit behind newer ones indefinitely and
// the queue would need a sweep to reach it.
func TestReadyAttemptsAreServedOldestFirst(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)

	// Inserted newest-first, so a queue that returned insertion order
	// would fail this.
	newest := seedAttempt(t, s, binding, "msg-newest", "job-newest")
	middle := seedAttempt(t, s, binding, "msg-middle", "job-middle")
	oldest := seedAttempt(t, s, binding, "msg-oldest", "job-oldest")
	now := time.Now()
	backdateAttempt(t, s, newest, now.Add(-time.Minute))
	backdateAttempt(t, s, middle, now.Add(-time.Hour))
	backdateAttempt(t, s, oldest, now.Add(-24*time.Hour))

	var ready []Attempt
	inTx(t, s, func(tx *Tx) error {
		var err error
		ready, err = tx.ReadyAttempts(binding)
		return err
	})

	got := make([]string, len(ready))
	for i, a := range ready {
		got[i] = a.SourceWorkloadKey
	}
	want := []string{"job-oldest", "job-middle", "job-newest"}
	if !slices.Equal(got, want) {
		t.Errorf("ready queue = %v; want %v, so the oldest attempt is always the next served", got, want)
	}
}

// serve leases an attempt and walks that lease to released, which is what
// one serving of an attempt costs. Requeue happens in the same commit as
// the release, so the next serving always starts with the predecessor
// already terminal.
func serve(t *testing.T, s *Store, binding int64, attemptID string) Lease {
	t.Helper()
	var lease Lease
	inTx(t, s, func(tx *Tx) error {
		var err error
		lease, err = tx.LeaseAttempt(attemptID, binding, "standard")
		return err
	})
	for _, step := range [][2]LeaseState{
		{LeaseReserved, LeaseProvisioning},
		{LeaseProvisioning, LeaseFailed},
		{LeaseFailed, LeaseCleaning},
		{LeaseCleaning, LeaseReleased},
	} {
		inTx(t, s, func(tx *Tx) error { return tx.TransitionLease(lease.ID, step[0], step[1]) })
	}
	return lease
}

// TestARequeuedAttemptIsServedAgain. Requeue returns an attempt whose work
// provably never began to the servable queue, and a servable queue is one
// the scheduler leases from. The schema said one lease per attempt ever,
// so the second lease failed on the constraint, the scheduler logged and
// skipped, and the attempt sat at the head of an age-ordered queue that
// nothing could drain.
func TestARequeuedAttemptIsServedAgain(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attemptID := seedAttempt(t, s, binding, "msg-1", "job-1")

	first := serve(t, s, binding, attemptID)
	inTx(t, s, func(tx *Tx) error { return tx.Requeue(attemptID) })

	var second Lease
	inTx(t, s, func(tx *Tx) error {
		var err error
		second, err = tx.LeaseAttempt(attemptID, binding, "standard")
		if err != nil {
			t.Fatalf("the requeued attempt could not be served again: %v", err)
		}
		return nil
	})
	if second.ID == first.ID {
		t.Error("the second serving reused the first lease; each serving is its own")
	}

	// The predecessor is history, not a hole: it is what says the attempt
	// was served before, and the late observations of that run correlate
	// through it.
	inTx(t, s, func(tx *Tx) error {
		got, err := tx.LeaseByAttempt(attemptID)
		if err != nil {
			return err
		}
		if got.ID != second.ID {
			t.Errorf("LeaseByAttempt returned %q; want the newest, %q", got.ID, second.ID)
		}
		n, err := tx.servingsSoFar(attemptID)
		if err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("servings so far = %d; want 2, so the history is the count", n)
		}
		return nil
	})
}

// TestOnlyOneLeaseOfAnAttemptIsLive is the other half: the constraint was
// wrong about "ever", not about "at once". Two live leases would be two
// capsules for one workload, which is the invariant nothing else checks.
func TestOnlyOneLeaseOfAnAttemptIsLive(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attemptID := seedAttempt(t, s, binding, "msg-1", "job-1")

	inTx(t, s, func(tx *Tx) error {
		_, err := tx.LeaseAttempt(attemptID, binding, "standard")
		return err
	})
	// Straight past the claim's compare-and-swap, so the index is the only
	// thing left to refuse it.
	err := s.Tx(t.Context(), func(tx *Tx) error {
		_, err := tx.tx.Exec(
			`INSERT INTO capsule_leases (id, binding_id, attempt_id, tier_id, state) VALUES (?, ?, ?, ?, ?)`,
			"lease-second", binding, attemptID, "standard", LeaseReserved)
		return err
	})
	if err == nil {
		t.Error("a second live lease was accepted; one attempt would be served by two capsules")
	}
}

// TestTheRetryBudgetEndsInReviewRatherThanForever. Nothing bounded how
// many times an attempt could be requeued: a failure that repeats -- an
// image that will not pull, a daemon that will not answer -- burned a
// capsule, a runner registration and a lane on every pass, forever.
func TestTheRetryBudgetEndsInReviewRatherThanForever(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attemptID := seedAttempt(t, s, binding, "msg-1", "job-1")

	for i := 1; i < DefaultRetryBudget; i++ {
		serve(t, s, binding, attemptID)
		inTx(t, s, func(tx *Tx) error {
			if err := tx.Requeue(attemptID); err != nil {
				t.Fatalf("serving %d was refused within the budget: %v", i, err)
			}
			return nil
		})
	}
	serve(t, s, binding, attemptID)

	err := s.Tx(t.Context(), func(tx *Tx) error { return tx.Requeue(attemptID) })
	if !errors.Is(err, ErrRetryBudgetExhausted) {
		t.Fatalf("the requeue past the budget returned %v; want ErrRetryBudgetExhausted, "+
			"or a failure that repeats is retried without end", err)
	}
}

// TestAnOperatorRetryIsNotOverruledByTheBudget. The budget answers "will
// this ever stop", which an operator resolving a review has already
// answered. It also clears the evidence: the serving under review is over,
// and the next one starts from nothing.
func TestAnOperatorRetryIsNotOverruledByTheBudget(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attemptID := seedAttempt(t, s, binding, "msg-1", "job-1")

	for i := 0; i < DefaultRetryBudget; i++ {
		serve(t, s, binding, attemptID)
		if i < DefaultRetryBudget-1 {
			inTx(t, s, func(tx *Tx) error { return tx.Requeue(attemptID) })
		}
	}
	inTx(t, s, func(tx *Tx) error {
		if err := tx.RecordEvidence(attemptID, EvidenceRuntimePrepared); err != nil {
			return err
		}
		if err := tx.RecordEvidence(attemptID, EvidenceStartAuthorized); err != nil {
			return err
		}
		return tx.HoldForReview(attemptID, ReviewReasonStartOutcomeUnknown)
	})

	inTx(t, s, func(tx *Tx) error {
		return tx.ResolveReviewToReady(attemptID, "operator", "retried")
	})
	inTx(t, s, func(tx *Tx) error {
		a, err := tx.Get(attemptID)
		if err != nil {
			return err
		}
		if a.State != "ready" {
			t.Errorf("after an operator retry the attempt is %q; the budget overruled a human", a.State)
		}
		if a.Evidence != EvidenceNotStarted {
			t.Errorf("evidence after an operator retry is %q; want %q, or the next serving's "+
				"first honest observation is a write that moves backwards",
				a.Evidence, EvidenceNotStarted)
		}
		return nil
	})
}

// TestARetryInFlightIsNotStranded. "Stranded" meant an open attempt with
// a released lease, which was unambiguous while an attempt could only
// ever hold one. It holds one per serving now, so a retry in flight has
// its predecessor released and its own lease live -- and repairing that
// is a restart tearing down the serving it has just started.
func TestARetryInFlightIsNotStranded(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	attemptID := seedAttempt(t, s, binding, "msg-1", "job-1")

	serve(t, s, binding, attemptID)
	inTx(t, s, func(tx *Tx) error { return tx.Requeue(attemptID) })
	inTx(t, s, func(tx *Tx) error {
		_, err := tx.LeaseAttempt(attemptID, binding, "standard")
		return err
	})

	inTx(t, s, func(tx *Tx) error {
		stranded, err := tx.StrandedAttempts()
		if err != nil {
			return err
		}
		for _, a := range stranded {
			if a.ID == attemptID {
				t.Error("an attempt being served right now was reported stranded; " +
					"repairing it tears down the lease that is serving it")
			}
		}
		return nil
	})

	// And once that serving ends without disposing of the attempt, it is
	// stranded again -- which is the case the query exists for.
	inTx(t, s, func(tx *Tx) error {
		live, err := tx.LeaseByAttempt(attemptID)
		if err != nil {
			return err
		}
		for _, step := range [][2]LeaseState{
			{LeaseReserved, LeaseProvisioning},
			{LeaseProvisioning, LeaseFailed},
			{LeaseFailed, LeaseCleaning},
			{LeaseCleaning, LeaseReleased},
		} {
			if err := tx.TransitionLease(live.ID, step[0], step[1]); err != nil {
				return err
			}
		}
		return nil
	})
	inTx(t, s, func(tx *Tx) error {
		stranded, err := tx.StrandedAttempts()
		if err != nil {
			return err
		}
		for _, a := range stranded {
			if a.ID == attemptID {
				return nil
			}
		}
		t.Error("an open attempt whose every lease is released was not reported stranded; " +
			"nothing else will ever look at it")
		return nil
	})
}

// TestARequeueClearsTheAuthorizationItOutlived. Recording the start
// authorization commits before the best-effort walk to starting, so an
// attempt can sit in prepared - inside the plain requeue's guard - while
// its evidence already says execution_start_authorized. Both requeue
// shapes must send the evidence back with the serving: left behind, the
// next serving's first honest observation is a write that moves
// backwards, and the retry deterministically burns a capsule and lands
// in review under the wrong reason.
func TestARequeueClearsTheAuthorizationItOutlived(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		requeue func(*Tx, string) error
	}{
		{"plain requeue from prepared", "prepared", func(tx *Tx, id string) error {
			if err := tx.Advance(id, "leased", "preparing"); err != nil {
				return err
			}
			if err := tx.Advance(id, "preparing", "prepared"); err != nil {
				return err
			}
			return tx.Requeue(id)
		}},
		{"proven inert from starting", "starting", func(tx *Tx, id string) error {
			for _, step := range [][2]string{
				{"leased", "preparing"}, {"preparing", "prepared"}, {"prepared", "starting"},
			} {
				if err := tx.Advance(id, step[0], step[1]); err != nil {
					return err
				}
			}
			return tx.RequeueProvenInert(id)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			binding := seedBinding(t, s)
			attemptID := seedAttempt(t, s, binding, "msg-1", "job-1")
			inTx(t, s, func(tx *Tx) error {
				if _, err := tx.LeaseAttempt(attemptID, binding, "standard"); err != nil {
					return err
				}
				for _, e := range []Evidence{EvidenceRuntimePrepared, EvidenceStartAuthorized} {
					if err := tx.RecordEvidence(attemptID, e); err != nil {
						return err
					}
				}
				return tc.requeue(tx, attemptID)
			})
			inTx(t, s, func(tx *Tx) error {
				if err := tx.RecordEvidence(attemptID, EvidenceRuntimePrepared); err != nil {
					t.Errorf("the second serving's first observation was refused: %v", err)
				}
				return nil
			})
		})
	}
}

// TestRetryBudgetIsConfigurable: the budget breaks a loop rather than
// tunes a rate, so what matters is that a deployment can set it at all
// and that nothing can set it to nothing — a count below one is not a
// smaller budget, it is the unbounded retry the counter exists to close.
func TestRetryBudgetIsConfigurable(t *testing.T) {
	for name, tc := range map[string]struct {
		set  int
		want int
	}{
		"unset keeps the default": {0, DefaultRetryBudget},
		"a deployment's own":      {7, 7},
		"one is the floor":        {1, 1},
		"below the floor is no budget at all, and is ignored": {0, DefaultRetryBudget},
		"a negative count is ignored too":                     {-3, DefaultRetryBudget},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir(), tc.set)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.retryBudget != tc.want {
				t.Errorf("retryBudget = %d, want %d", s.retryBudget, tc.want)
			}
		})
	}
}

// TestAttemptProviderReferences: the CLI's own help for resolving a held
// attempt tells an operator to verify externally, in the provider's UI,
// that the workload never executed. Nothing exposed the identifiers that
// check needs, so the procedure the tool prescribed could not be carried
// out from the tool.
func TestAttemptProviderReferences(t *testing.T) {
	s := newStore(t)
	attemptID := seedAttempt(t, s, seedBinding(t, s), "msg-1", "job-1")

	// Before the provider says anything, there is nothing to point at.
	inTx(t, s, func(tx *Tx) error {
		refs, err := tx.AttemptProviderReferences(attemptID)
		if err != nil {
			return err
		}
		if refs != nil {
			t.Errorf("references = %v; want none before the provider recorded any", refs)
		}
		return nil
	})

	inTx(t, s, func(tx *Tx) error {
		return tx.RecordGitHubAttemptMetadata(attemptID, "job-1", 0, 987654)
	})
	inTx(t, s, func(tx *Tx) error {
		refs, err := tx.AttemptProviderReferences(attemptID)
		if err != nil {
			return err
		}
		if refs["workflow_run_id"] != "987654" || refs["job_id"] != "job-1" {
			t.Errorf("references = %v; want the run this attempt belongs to", refs)
		}
		// A zero was never observed, and reporting it would read as one
		// that was.
		if _, ok := refs["runner_request_id"]; ok {
			t.Errorf("references = %v; want the unrecorded identifier absent", refs)
		}
		return nil
	})
}
