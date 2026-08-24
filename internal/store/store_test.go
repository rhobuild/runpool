package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"

	"github.com/rhobuild/runpool/internal/assignment"
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

func seedBinding(t *testing.T, s *Store) assignment.BindingID {
	t.Helper()
	var id assignment.BindingID
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
func seedAttempt(t *testing.T, s *Store, binding assignment.BindingID,
	deliveryKey string, workloadKey assignment.SourceWorkloadKey) assignment.AttemptID {
	t.Helper()
	var id string
	inTx(t, s, func(tx *Tx) error {
		if _, err := tx.RecordDelivery(binding, deliveryKey, fingerprint(deliveryKey),
			[]WorkloadRow{{SourceWorkloadKey: string(workloadKey), TenantKey: "acme", ProjectKey: "app"}}); err != nil {
			return err
		}
		attempt, err := tx.q.GetOpenAttemptByWorkload(tx.ctx, sqlitedb.GetOpenAttemptByWorkloadParams{
			BindingID: int64(binding), SourceWorkloadKey: string(workloadKey),
		})
		id = attempt.ID
		return err
	})
	return assignment.AttemptID(id)
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

	var first assignment.DeliveryID
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
		if again != first {
			t.Errorf("redelivery produced delivery %d; want the original %d", again, first)
		}
		n, err := tx.q.CountOpenAttempts(tx.ctx, int64(binding))
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
		if err := tx.SupersedeOpenAttempt(binding, "job-r", assignment.ResolutionSuperseded, 0); err != nil {
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
		n, err := tx.q.CountOpenAttempts(tx.ctx, int64(binding))
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

	var deliveryID assignment.DeliveryID
	inTx(t, s, func(tx *Tx) error {
		var err error
		deliveryID, err = tx.RecordDelivery(binding, "msg-ack", fingerprint("ack"),
			[]WorkloadRow{{SourceWorkloadKey: "job-ack", TenantKey: "acme", ProjectKey: "app"}})
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

	var leaseID assignment.LeaseID
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
		if err := tx.Settle(attempt, AttemptLeased, assignment.ResolutionCompletedObserved); err != nil {
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

	var deliveryID assignment.DeliveryID
	inTx(t, s, func(tx *Tx) error {
		var err error
		deliveryID, err = tx.RecordDelivery(binding, "msg-wedge", fingerprint("wedge"),
			[]WorkloadRow{{SourceWorkloadKey: "job-wedge", TenantKey: "acme", ProjectKey: "app"}})
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
	finishOrder := make([]assignment.LeaseID, released)
	for i := range released {
		id, _ := releasedLease(t, s, binding, assignment.SourceWorkloadKey(fmt.Sprintf("done-%03d", i)))
		backdateLease(t, s, id, base, base.Add(time.Duration(i)*time.Hour))
		finishOrder[i] = id
	}

	// Three live leases, each in a different state.
	live := map[assignment.LeaseID]bool{}
	for i, state := range []LeaseState{LeaseReserved, LeaseProvisioning, LeaseRuntimeRegistered} {
		key := assignment.SourceWorkloadKey(fmt.Sprintf("live-%d", i))
		attempt := seedAttempt(t, s, binding, "msg-"+string(key), "job-"+key)
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
	seen := map[assignment.LeaseID]bool{}
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
	var carried []assignment.LeaseID
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
func backdateLease(t *testing.T, s *Store, leaseID assignment.LeaseID, started, finished time.Time) {
	t.Helper()
	inTx(t, s, func(tx *Tx) error {
		_, err := tx.tx.Exec(
			`UPDATE capsule_leases SET created_at = ?, updated_at = ? WHERE id = ?`,
			started.Unix(), finished.Unix(), leaseID)
		return err
	})
}

// releasedLease drives one attempt to a released lease and returns both ids.
func releasedLease(t *testing.T, s *Store, binding assignment.BindingID, key assignment.SourceWorkloadKey) (leaseID assignment.LeaseID, attemptID assignment.AttemptID) {
	t.Helper()
	attemptID = seedAttempt(t, s, binding, "msg-"+string(key), "job-"+key)
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
		if err := tx.Settle(settledAttempt, AttemptLeased, assignment.ResolutionCompletedObserved); err != nil {
			return err
		}
		return tx.Settle(resolvedAttempt, AttemptLeased, assignment.ResolutionCompletedObserved)
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
		return tx.Settle(prunableAttempt, AttemptLeased, assignment.ResolutionCompletedObserved)
	})

	// Finished, but recent: outside the window.
	recent, recentAttempt := releasedLease(t, s, binding, "recent")
	inTx(t, s, func(tx *Tx) error {
		return tx.Settle(recentAttempt, AttemptLeased, assignment.ResolutionCompletedObserved)
	})

	// Old and released, but its attempt is still open — the crash window.
	stranded, _ := releasedLease(t, s, binding, "stranded")
	backdateLease(t, s, stranded, old, old)

	// Old and released, but cleanup never finished: an intent survives.
	wedged, wedgedAttempt := releasedLease(t, s, binding, "wedged")
	backdateLease(t, s, wedged, old, old)
	inTx(t, s, func(tx *Tx) error {
		if err := tx.Settle(wedgedAttempt, AttemptLeased, assignment.ResolutionCompletedObserved); err != nil {
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
		if attempt.State != AttemptSettled {
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
func backdateAttempt(t *testing.T, s *Store, attemptID assignment.AttemptID, arrived time.Time) {
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
		got[i] = string(a.SourceWorkloadKey)
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
func serve(t *testing.T, s *Store, binding int64, attemptID assignment.AttemptID) Lease {
	t.Helper()
	var lease Lease
	inTx(t, s, func(tx *Tx) error {
		var err error
		lease, err = tx.LeaseAttempt(attemptID, assignment.BindingID(binding), "standard")
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

	first := serve(t, s, int64(binding), attemptID)
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
		serve(t, s, int64(binding), attemptID)
		inTx(t, s, func(tx *Tx) error {
			if err := tx.Requeue(attemptID); err != nil {
				t.Fatalf("serving %d was refused within the budget: %v", i, err)
			}
			return nil
		})
	}
	serve(t, s, int64(binding), attemptID)

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
		serve(t, s, int64(binding), attemptID)
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
		if a.State != AttemptReady {
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

	serve(t, s, int64(binding), attemptID)
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
		requeue func(*Tx, string) error
	}{
		{"plain requeue from prepared", func(tx *Tx, id string) error {
			if err := tx.Advance(assignment.AttemptID(id), AttemptLeased, AttemptPreparing); err != nil {
				return err
			}
			if err := tx.Advance(assignment.AttemptID(id), AttemptPreparing, AttemptPrepared); err != nil {
				return err
			}
			return tx.Requeue(assignment.AttemptID(id))
		}},
		{"proven inert from starting", func(tx *Tx, id string) error {
			for _, step := range [][2]AttemptState{
				{AttemptLeased, AttemptPreparing}, {AttemptPreparing, AttemptPrepared}, {AttemptPrepared, AttemptStarting},
			} {
				if err := tx.Advance(assignment.AttemptID(id), step[0], step[1]); err != nil {
					return err
				}
			}
			return tx.RequeueProvenInert(assignment.AttemptID(id))
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
				return tc.requeue(tx, string(attemptID))
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

// A held attempt must not stall its binding's whole ordered queue, and a
// held attempt whose start was authorized must not be replaced.
//
// manual_review is an open state, so the partial unique index refuses
// every redelivery of that workload while one sits there — the queue is
// ordered, so the binding stops. The provider reassigning the workload
// is what makes a pending human decision moot, so the review gives way.
//
// Except when it cannot: a review is reachable from `starting` and
// `running`, and `start_outcome_unknown` is precisely the review of an
// attempt whose start was authorized and whose outcome nobody could
// prove. Replacing that one runs work that may already have run, which
// no queue property is worth. The evidence is what separates the two.
func TestSupersedingAHeldAttemptTurnsOnWhatItConsumed(t *testing.T) {
	for _, tc := range []struct {
		name           string
		evidence       Evidence
		reason         ReviewReason
		wantSuperseded bool
	}{
		{"nothing was prepared", EvidenceNotStarted, ReviewReasonIncompatibleCapsule, true},
		{"a runtime was prepared", EvidenceRuntimePrepared, ReviewReasonRetryBudgetExhausted, true},
		{"a start was authorized", EvidenceStartAuthorized, ReviewReasonStartOutcomeUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			binding := seedBinding(t, s)
			workloads := []WorkloadRow{{SourceWorkloadKey: "job-held", TenantKey: "acme", ProjectKey: "app"}}

			var attemptID assignment.AttemptID
			inTx(t, s, func(tx *Tx) error {
				if _, err := tx.RecordDelivery(binding, "msg-1", fingerprint("first"), workloads); err != nil {
					return err
				}
				ready, err := tx.ReadyAttempts(binding)
				if err != nil {
					return err
				}
				attemptID = ready[0].ID
				if err := tx.RecordEvidence(attemptID, tc.evidence); err != nil {
					return err
				}
				return tx.HoldForReview(attemptID, tc.reason)
			})

			err := s.Tx(t.Context(), func(tx *Tx) error {
				serr := tx.SupersedeOpenAttempt(binding, "job-held", assignment.ResolutionSuperseded, 0)
				if serr != nil {
					return serr
				}
				_, rerr := tx.RecordDelivery(binding, "msg-2", fingerprint("second"), workloads)
				return rerr
			})

			var got Attempt
			inTx(t, s, func(tx *Tx) error {
				var err error
				got, err = tx.Get(attemptID)
				return err
			})

			if tc.wantSuperseded {
				if err != nil {
					t.Fatalf("the redelivery was refused: %v", err)
				}
				if got.State != AttemptSuperseded {
					t.Errorf("held attempt = %s; want superseded so the queue moves", got.State)
				}
				return
			}
			if err == nil {
				t.Fatal("the redelivery replaced an attempt whose start was authorized")
			}
			if got.State != AttemptManualReview {
				t.Errorf("held attempt = %s; want it left in manual_review", got.State)
			}
		})
	}
}

// A binding configuration no longer claims is forgotten, unless it still
// owns work.
//
// A renamed scale set or a removed tier leaves a row nothing serves: it
// appears in every report and no command removes it. But a binding that
// owns a delivery is the trail of work that ran, and a report that lost
// it could not say whose work it was — so that one is kept whatever the
// configuration says.
func TestABindingConfigurationNoLongerClaimsIsForgotten(t *testing.T) {
	s := newStore(t)

	var kept, dropped, withWork assignment.BindingID
	inTx(t, s, func(tx *Tx) error {
		var err error
		if kept, err = tx.EnsureBinding("app", "github_actions", "v2|app|default|runpool-standard"); err != nil {
			return err
		}
		if dropped, err = tx.EnsureBinding("app", "github_actions", "v2|app|default|runpool-renamed"); err != nil {
			return err
		}
		if withWork, err = tx.EnsureBinding("old", "github_actions", "v2|old|default|runpool-old"); err != nil {
			return err
		}
		_, err = tx.RecordDelivery(withWork, "msg-old", fingerprint("old"),
			[]WorkloadRow{{SourceWorkloadKey: "job-old", TenantKey: "acme", ProjectKey: "old"}})
		return err
	})

	var forgotten int
	inTx(t, s, func(tx *Tx) error {
		var err error
		forgotten, err = tx.ForgetUnclaimedBindings([]assignment.BindingID{kept})
		return err
	})
	if forgotten != 1 {
		t.Errorf("forgot %d bindings; want exactly the one that holds no work", forgotten)
	}

	var ids []assignment.BindingID
	inTx(t, s, func(tx *Tx) error {
		rows, err := tx.Bindings()
		if err != nil {
			return err
		}
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
		return nil
	})
	if !slices.Contains(ids, kept) {
		t.Error("a claimed binding was forgotten")
	}
	if slices.Contains(ids, dropped) {
		t.Error("a binding configuration no longer claims is still reported")
	}
	if !slices.Contains(ids, withWork) {
		t.Error("a binding that still owns a delivery was forgotten with the work it explains")
	}

	// An empty claim is a caller with no bindings at all, which serve
	// refuses before reaching here; deleting everything would turn that
	// mistake into data loss.
	inTx(t, s, func(tx *Tx) error {
		n, err := tx.ForgetUnclaimedBindings(nil)
		if err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("an empty claim forgot %d bindings", n)
		}
		return nil
	})
}

// TestTheRetryCountIsIndexed: the count that decides whether a workload
// is served again is answered from an index, not a scan.
//
// It runs inside the write transaction that decides a requeue, on the
// one connection every other writer waits for, and it counts released
// leases too — so the partial unique index cannot serve it, since that
// one excludes exactly the rows the count is about. Without an index of
// its own the check scans every lease the host has ever recorded, and
// that cost grows with history rather than with live work.
func TestTheRetryCountIsIndexed(t *testing.T) {
	s := newStore(t)
	var plan []string
	inTx(t, s, func(tx *Tx) error {
		rows, err := tx.tx.Query(
			`EXPLAIN QUERY PLAN SELECT count(*) FROM capsule_leases WHERE attempt_id = ?`, "att-x")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		return rows.Err()
	})
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "USING INDEX") && !strings.Contains(joined, "USING COVERING INDEX") {
		t.Errorf("the retry count plans as %q; want an index, not a scan of every lease ever recorded", joined)
	}
}

// TestOnlyAnUnstartedAttemptOfThisServingIsAuthorized: the one
// authoritative edge in the walk accepts exactly the states a serving
// passes through before its start.
//
// The edges before it are observability. They are written outside the
// transaction that matters and a failure is logged rather than retried,
// because their only reader is a person — so an attempt can legitimately
// be a state or two behind when the start is authorized. Requiring the
// exact predecessor made a lost observability write tear down a prepared
// capsule and burn a serving.
//
// What it must still refuse is every state somebody else could have put
// the attempt in, and every state that means a start already happened. A
// second authorization is a second run of the same job.
//
// The table is the specification. It is checked against the schema's own
// CHECK constraint, so a state added to the product without a decision
// here fails rather than defaulting to one.
func TestOnlyAnUnstartedAttemptOfThisServingIsAuthorized(t *testing.T) {
	cases := []struct {
		state     AttemptState
		authorize bool
		because   string
	}{
		{"ready", false, "an operator returned it to the queue; a new serving claims it"},
		{"leased", true, "this serving's, with both walk edges lost"},
		{"preparing", true, "this serving's, with the edge into prepared lost"},
		{"prepared", true, "this serving's, with nothing lost"},
		{"starting", false, "already authorized; a second start is a second run"},
		{"running", false, "already started; a second start is a second run"},
		{"superseded", false, "a redelivery replaced it and its successor is serving"},
		{"canceled", false, "the source withdrew the workload"},
		{"settled", false, "already resolved"},
		{"manual_review", false, "held for a person who has not decided yet"},
	}

	// The universe comes from the one list, which its own test holds
	// against the schema.
	decided := make(map[AttemptState]bool, len(cases))
	for _, c := range cases {
		decided[c.state] = true
	}
	for _, state := range AllAttemptStates {
		if !decided[state] {
			t.Errorf("an attempt can be %q and this test decides nothing about starting it", state)
		}
	}

	s := newStore(t)
	binding := seedBinding(t, s)

	// Each case gets a real lease, because the authorization is scoped to
	// one: an attempt in a servable state is not enough on its own.
	leased := func(t *testing.T, name string, state AttemptState) (assignment.LeaseID, assignment.AttemptID) {
		t.Helper()
		id := seedAttempt(t, s, binding, "msg-"+name, assignment.SourceWorkloadKey("job-"+name))
		var leaseID assignment.LeaseID
		inTx(t, s, func(tx *Tx) error {
			lease, err := tx.LeaseAttempt(id, binding, "tier-a")
			if err != nil {
				return err
			}
			leaseID = lease.ID
			if state == AttemptLeased {
				return nil
			}
			return tx.Advance(id, AttemptLeased, state)
		})
		return leaseID, id
	}

	for _, c := range cases {
		t.Run(string(c.state), func(t *testing.T) {
			leaseID, id := leased(t, string(c.state), c.state)

			var err error
			inTx(t, s, func(tx *Tx) error {
				err = tx.AuthorizeStart(leaseID, id)
				return nil
			})
			switch {
			case c.authorize && err != nil:
				t.Errorf("AuthorizeStart from %s refused (%v); it must be allowed — %s", c.state, err, c.because)
			case !c.authorize && err == nil:
				t.Errorf("AuthorizeStart from %s was allowed; it must be refused — %s", c.state, c.because)
			case !c.authorize && !errors.Is(err, ErrConflict):
				t.Errorf("AuthorizeStart from %s failed with %v; want ErrConflict", c.state, err)
			}
		})
	}

	// The state set says nobody has resolved the attempt. It does not say
	// the attempt is still this lease's, and that is the half that keeps
	// a job from being run twice.
	t.Run("a lease that has been released", func(t *testing.T) {
		leaseID, id := leased(t, "released-lease", AttemptPrepared)
		// reserved -> failed -> cleaning -> released is the shortest real
		// path to a released lease; the state machine refuses shortcuts.
		inTx(t, s, func(tx *Tx) error {
			for _, edge := range [][2]LeaseState{
				{LeaseReserved, LeaseFailed},
				{LeaseFailed, LeaseCleaning},
				{LeaseCleaning, LeaseReleased},
			} {
				if err := tx.TransitionLease(leaseID, edge[0], edge[1]); err != nil {
					return err
				}
			}
			return nil
		})
		inTx(t, s, func(tx *Tx) error {
			if err := tx.AuthorizeStart(leaseID, id); !errors.Is(err, ErrConflict) {
				t.Errorf("AuthorizeStart on a released lease = %v; want ErrConflict", err)
			}
			return nil
		})
	})

	t.Run("a lease that never held this attempt", func(t *testing.T) {
		_, id := leased(t, "wrong-lease-a", AttemptPrepared)
		other, _ := leased(t, "wrong-lease-b", AttemptPrepared)
		inTx(t, s, func(tx *Tx) error {
			if err := tx.AuthorizeStart(other, id); !errors.Is(err, ErrConflict) {
				t.Errorf("AuthorizeStart with another lease's id = %v; want ErrConflict", err)
			}
			return nil
		})
	})
}

// TestEveryLeaseKeepsItsAttempt: two leases that served one attempt each
// get their own row in the report.
//
// The index that forbids two live leases per attempt says nothing about
// a released one, so an attempt returned to the queue and served again
// is named by both. Indexing the lookup by attempt made them collapse
// into one entry, and the snapshot appends released leases after live
// ones — so the entry that survived belonged to the lease that had
// finished, and the lease still running was the one reported as serving
// nothing.
func TestEveryLeaseKeepsItsAttempt(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	id := seedAttempt(t, s, binding, "msg-two-leases", "job-two-leases")

	var first, second Lease
	inTx(t, s, func(tx *Tx) error {
		l, err := tx.LeaseAttempt(id, binding, "tier-a")
		if err != nil {
			return err
		}
		first = l
		for _, edge := range [][2]LeaseState{
			{LeaseReserved, LeaseFailed},
			{LeaseFailed, LeaseCleaning},
			{LeaseCleaning, LeaseReleased},
		} {
			if err := tx.TransitionLease(first.ID, edge[0], edge[1]); err != nil {
				return err
			}
		}
		// Back to the queue and served again, which is the whole of how
		// one attempt comes to have two leases.
		if err := tx.Advance(id, AttemptLeased, AttemptReady); err != nil {
			return err
		}
		second, err = tx.LeaseAttempt(id, binding, "tier-a")
		return err
	})

	// Live first, released after: the order Snapshot builds.
	var got map[assignment.LeaseID]Attempt
	inTx(t, s, func(tx *Tx) error {
		var err error
		got, err = tx.attemptsOfLeases([]Lease{second, first})
		return err
	})

	for _, l := range []Lease{second, first} {
		a, ok := got[l.ID]
		if !ok {
			t.Errorf("lease %s has no attempt in the report; it serves %s", l.ID, l.AttemptID)
			continue
		}
		if a.ID != id {
			t.Errorf("lease %s reports attempt %s; want %s", l.ID, a.ID, id)
		}
	}
}

// TestTheSetReadAgreesWithTheGeneratedQuery: the hand-written column
// list and the generated one return the same attempt.
//
// sqlc's sqlite engine has no slice parameter, so the report's set read
// cannot be generated and repeats the projection by hand. Nothing makes
// the two share a definition, and nothing about them fails to compile
// when they disagree: a column added to one is a scan error at best and
// a shifted value at worst, and two columns of the same type swapped in
// one list is silent in both.
//
// Every column below holds a value unique among the columns it could be
// confused with, so any swap changes the result. A row driven there by
// the state machine instead would leave several columns equal, and the
// swaps between those are exactly the ones no compiler can catch.
func TestTheSetReadAgreesWithTheGeneratedQuery(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	id := seedAttempt(t, s, binding, "msg-parity", "job-parity")

	var lease Lease
	inTx(t, s, func(tx *Tx) error {
		l, err := tx.LeaseAttempt(id, binding, "tier-a")
		lease = l
		return err
	})

	// A distinct value in every column, written directly. Driving the
	// row there through the state machine leaves columns that cannot be
	// told apart: resolution and review_reason are both closed
	// vocabularies whose columns constrain them -- so the sentinels for
	// those two are real members, distinct from each other and from
	// every other column's -- reviewed_at and settled_at
	// are both null until one happens, and reviewed_at and received_at
	// are both unixepoch() in the same second when one does. Any of
	// those pairs could be swapped in one list and not the other, and
	// nothing would notice — which is the whole of what this test is
	// for.
	inTx(t, s, func(tx *Tx) error {
		_, err := tx.tx.Exec(`UPDATE assignment_attempts SET
			source_workload_key = 'workload-value',
			tenant_key          = 'tenant-value',
			project_key         = 'project-value',
			state               = 'manual_review',
			execution_evidence  = 'running_observed',
			resolution          = 'superseded',
			review_reason       = 'capsule_incompatible',
			reviewed_by         = 'reviewed-by-value',
			reviewed_at         = 111,
			received_at         = 222,
			settled_at          = 333
			WHERE id = ?`, string(id))
		return err
	})

	var generated, set Attempt
	inTx(t, s, func(tx *Tx) error {
		row, err := tx.q.GetAttempt(tx.ctx, string(id))
		if err != nil {
			return err
		}
		generated = fromRow(row)
		byLease, err := tx.attemptsOfLeases([]Lease{lease})
		if err != nil {
			return err
		}
		set = byLease[lease.ID]
		return nil
	})

	if generated != set {
		t.Errorf("the two projections disagree:\n generated: %+v\n set read:  %+v", generated, set)
	}
}

// TestAttemptStatesCoverTheSchema: the list every caller decides from is
// the set the database actually allows.
//
// A list beside a constraint is a list that drifts from it, and the
// drift is silent in the direction that matters: a state the schema
// gains and the list does not is a state every table test skips while
// reporting itself total.
func TestAttemptStatesCoverTheSchema(t *testing.T) {
	schema, err := migrationsFS.ReadFile("migrations/000001_initial.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(schema)
	start := strings.Index(body, "CHECK (state IN ('ready'")
	if start < 0 {
		t.Fatal("the attempt state CHECK constraint moved; this test can no longer find it")
	}
	constraint := body[start : start+strings.Index(body[start:], "))")]

	listed := make(map[AttemptState]bool, len(AllAttemptStates))
	for _, s := range AllAttemptStates {
		listed[s] = true
	}
	found := 0
	for _, state := range strings.Split(constraint, "'") {
		if len(state) == 0 || strings.ContainsAny(state, "(), \n\t") || state == "CHECK " {
			continue
		}
		found++
		if !listed[AttemptState(state)] {
			t.Errorf("the schema allows attempt state %q and AllAttemptStates omits it", state)
		}
	}
	if found != len(AllAttemptStates) {
		t.Errorf("the constraint names %d states and AllAttemptStates has %d; "+
			"one of them lists something the other does not", found, len(AllAttemptStates))
	}
}

// TestASecondReviewCycleIsItsOwnHistory: holding an attempt twice, and
// resolving it twice, leaves four decisions in the record rather than
// two.
//
// The attempt row keeps only the latest review reason, the latest actor
// and the latest resolution, so the event log is the whole of what says
// who decided what and why the time before. A fixed idempotency key made
// every occurrence after the first a replay: the insert's conflict
// clause dropped it, silently and successfully, and the earlier decision
// was the one that survived.
//
// The cycle is reachable — hold, an operator resolves back to the queue,
// the attempt is served again and held again — and one of this round's
// own changes is what made it complete rather than pin the lease.
func TestASecondReviewCycleIsItsOwnHistory(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	id := seedAttempt(t, s, binding, "msg-two-reviews", "job-two-reviews")

	cycle := func(reason ReviewReason, actor, why string) {
		t.Helper()
		inTx(t, s, func(tx *Tx) error {
			if err := tx.HoldForReview(id, reason); err != nil {
				return err
			}
			return tx.ResolveReviewToReady(id, why, actor)
		})
	}
	// Distinct reasons as well as distinct actors, so the hold half of
	// this proves the second hold was recorded rather than merely that
	// two rows exist.
	cycle(ReviewReasonStartOutcomeUnknown, "alice", "the runner never picked it up")
	cycle(ReviewReasonRetryBudgetExhausted, "bob", "the daemon was replaced mid-flight")

	var events []Event
	inTx(t, s, func(tx *Tx) error {
		var err error
		events, err = tx.Events(id)
		return err
	})

	var holds, resolves []string
	for _, e := range events {
		switch e.Kind {
		case "manual_review_requested":
			holds = append(holds, e.Detail)
		case "operator_resolved":
			resolves = append(resolves, e.Detail)
		}
	}
	if len(holds) != 2 {
		t.Errorf("%d hold(s) recorded for two reviews: %v", len(holds), holds)
	}
	held := strings.Join(holds, " ")
	for _, reason := range []ReviewReason{ReviewReasonStartOutcomeUnknown, ReviewReasonRetryBudgetExhausted} {
		if !strings.Contains(held, string(reason)) {
			t.Errorf("the record does not carry the hold for %s: %v", reason, holds)
		}
	}
	if len(resolves) != 2 {
		t.Fatalf("%d resolution(s) recorded for two decisions: %v", len(resolves), resolves)
	}
	// Both actors, not the first one twice and not the last one only.
	joined := strings.Join(resolves, " ")
	for _, actor := range []string{"alice", "bob"} {
		if !strings.Contains(joined, actor) {
			t.Errorf("the record does not name %s: %v", actor, resolves)
		}
	}
}

// TestUninstallClearsABindingThatReachedItsProvider: uninstall runs once
// and has to finish.
//
// The contact row is written by a binding's own poll loop, so every
// instance that ever reached its provider has one. It is a child of
// provider_bindings with the foreign key enforced, so leaving it behind
// fails the delete of its parent and aborts the whole transaction. By
// then the Docker objects are gone and the scale sets are deleted, and
// the operator is left with a half-removed instance and a state database
// no supported command will clear.
func TestUninstallClearsABindingThatReachedItsProvider(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	inTx(t, s, func(tx *Tx) error {
		return tx.RecordProviderContact(binding, time.Now())
	})

	if err := s.Tx(t.Context(), func(tx *Tx) error { return tx.PurgeEverything() }); err != nil {
		t.Fatalf("uninstall failed on a binding that had reached its provider: %v", err)
	}
	for _, table := range []string{"provider_binding_contact", "provider_bindings"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) after uninstall", table, n)
		}
	}
}

// seedEverything writes one row into every table an instance fills, so
// what uninstall does can be observed instead of read.
func seedEverything(t *testing.T, s *Store) {
	t.Helper()
	binding := seedBinding(t, s)
	attempt := seedAttempt(t, s, binding, "msg-seed", "job-seed")
	inTx(t, s, func(tx *Tx) error {
		if err := tx.RecordProviderContact(binding, time.Now()); err != nil {
			return err
		}
		if err := tx.RecordGitHubBindingMetadata(binding, "repository",
			"https://github.com/acme/app", "default", "runpool-standard", 41); err != nil {
			return err
		}
		// A second binding recorded before its scale set was ensured, so
		// its metadata row carries no scale set id. A statement narrowed
		// to the rows an instance provisioned would pass over exactly
		// this one, and pass over it in every real deployment too.
		unprovisioned, err := tx.EnsureBinding("default", "github_actions",
			"v1|repository|https://github.com/acme/other||runpool-standard")
		if err != nil {
			return err
		}
		if err := tx.RecordGitHubBindingMetadata(unprovisioned, "repository",
			"https://github.com/acme/other", "default", "runpool-standard", 0); err != nil {
			return err
		}
		if err := tx.RecordGitHubAttemptMetadata(attempt, "job-seed", 7, 9); err != nil {
			return err
		}
		if err := tx.RecordRepeatableEvent(attempt, EventManualReviewRequested, map[string]string{"why": "seeding"}); err != nil {
			return err
		}
		lease, err := tx.LeaseAttempt(attempt, binding, "standard")
		if err != nil {
			return err
		}
		if _, err := tx.PlanResource(lease.ID, ResourceContainer, "capsule", "runpool-seed"); err != nil {
			return err
		}
		project, err := tx.EnsureCacheProject("acme/app")
		if err != nil {
			return err
		}
		_, err = tx.LeaseCacheLane(project, "default", lease.ID, 2)
		return err
	})
}

// tablesOf is every table the schema declares.
func tablesOf(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func rowsIn(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestUninstallClearsEveryTableItOwns runs the purge against a database
// holding a row everywhere an instance writes one, and requires it to
// finish and to leave nothing of itself behind.
//
// It watches the database rather than reading the statements, which is
// what makes it total. Every way of getting this wrong arrives at the
// same two observations: a step that clears only some of its rows, one
// that names its table in a spelling the schema does not use, one
// declared in a form the generator emits differently, or a sequence
// that runs in an order the source does not show. Either a foreign key
// refuses a parent whose child is still present, or a table asserted
// empty is not.
func TestUninstallClearsEveryTableItOwns(t *testing.T) {
	s := newStore(t)

	// What uninstall deliberately leaves. meta carries the schema
	// fingerprint, and a database whose meta was cleared is refused on
	// every later open; pressure is one row whose retention keeps the
	// disk hysteresis across a restart; audit_log is a record of what an
	// operator did, which outlives the machine it describes.
	kept := []string{"audit_log", "meta", "pressure"}

	seedEverything(t, s)

	// The seed has to keep up with the schema, so an unseeded table is a
	// failure here rather than a silent gap: uninstall would be neither
	// observed against it nor observed to skip it.
	for _, table := range tablesOf(t, s) {
		if slices.Contains(kept, table) {
			continue
		}
		if rowsIn(t, s, table) == 0 {
			t.Errorf("nothing seeded %s, so what uninstall does to it is not observed; "+
				"seed it above, or say why uninstall leaves it", table)
		}
	}

	if err := s.Tx(t.Context(), func(tx *Tx) error { return tx.PurgeEverything() }); err != nil {
		t.Fatalf("uninstall did not finish: %v\nThe containers and the scale sets are already "+
			"gone by the time this runs, so what is left is a half-removed instance and a "+
			"state database no supported command will clear.", err)
	}

	for _, table := range tablesOf(t, s) {
		if slices.Contains(kept, table) {
			continue
		}
		if n := rowsIn(t, s, table); n != 0 {
			t.Errorf("%s still holds %d row(s) after uninstall; a reinstall onto a retained "+
				"state volume inherits them", table, n)
		}
	}
}

// TestEveryPurgeStatementIsTotal: uninstall names the whole instance, so
// each of its deletes takes a whole table.
//
// It reads the generated statements rather than the queries they came
// from, because those are what run: a document that reaches the
// generator damaged produces a statement nobody wrote, and comparing
// the two sources would not show it. A narrowed delete is the natural
// mistake here -- clearing only the rows an instance provisioned reads
// as caution -- and it leaves the rest behind for the foreign key of
// whatever they hang from to refuse.
func TestEveryPurgeStatementIsTotal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("sqlitedb", "purge.sql.go"))
	if err != nil {
		t.Fatal(err)
	}
	statements := regexp.MustCompile(`(?is)DELETE\s+FROM\s+([^\n`+"`"+`]*)`).FindAllStringSubmatch(string(raw), -1)
	if len(statements) < 2 {
		t.Fatalf("read %d delete statements out of the generated layer; "+
			"the rule below would hold vacuously", len(statements))
	}
	for _, stmt := range statements {
		rest := strings.TrimSpace(stmt[1])
		if fields := strings.Fields(rest); len(fields) != 1 {
			t.Errorf("uninstall runs `DELETE FROM %s`, which does not clear a whole table. "+
				"Every row it passes over stays behind for the foreign key of whatever "+
				"references it to refuse, and the delete of that parent is what aborts "+
				"the rest of the uninstall.", rest)
		}
	}
}

// TestForgettingABindingReachesEveryChildOfIt: the startup pass that
// drops bindings nobody claimed deletes the same parent uninstall does,
// so it needs the same children — and it runs on every start rather than
// once at the end of an instance's life.
func TestForgettingABindingReachesEveryChildOfIt(t *testing.T) {
	s := newStore(t)

	// Two children are deliberately not cleared. The pass selects only
	// bindings with no deliveries, so broker_deliveries is excluded by
	// its own query and needs no argument beyond that.
	//
	// capsule_leases is excluded on a narrower footing, worth stating
	// because the schema does not hold it: a lease carries binding_id and
	// attempt_id as two independent references, so a row naming a
	// delivery-free binding is accepted, and the pass would then fail on
	// every controller start. What keeps that from arising is the one
	// call site — attempts are read for a binding and leased under that
	// same binding — rather than a constraint. assignment_attempts, by
	// contrast, carries a composite reference for exactly this reason.
	//
	// Naming both here rather than leaving them out is the point: a child
	// that appears later and is neither cleared nor excluded for a stated
	// reason fails this test, which is the moment someone has to decide
	// which it is.
	excluded := []string{"broker_deliveries", "capsule_leases"}

	var children []string
	for child, parents := range foreignKeys(t, s) {
		if slices.Contains(parents, "provider_bindings") {
			children = append(children, child)
		}
	}
	if len(children) == 0 {
		t.Fatal("provider_bindings has no children in the schema; the rule proved nothing")
	}
	slices.Sort(children)

	for _, child := range children {
		switch {
		case slices.Contains(bindingChildren, child) && slices.Contains(excluded, child):
			t.Errorf("%s is both cleared and excluded; one of the two is wrong", child)
		case slices.Contains(bindingChildren, child), slices.Contains(excluded, child):
		default:
			t.Errorf("%s references provider_bindings and is neither cleared before a binding is "+
				"forgotten nor excluded for a reason; if the pass can meet a row of it, the delete "+
				"of the binding fails on every controller start", child)
		}
	}
	for _, cleared := range bindingChildren {
		if !slices.Contains(children, cleared) {
			t.Errorf("a binding is forgotten by clearing %s, which does not reference "+
				"provider_bindings; the statement deletes rows nothing required it to", cleared)
		}
	}
	for _, skipped := range excluded {
		if !slices.Contains(children, skipped) {
			t.Errorf("%s is excused from being cleared and does not reference provider_bindings; "+
				"the reason given above is about a table that is not in the way", skipped)
		}
	}
}

// foreignKeys maps each table to the tables it references, read from the
// database so a table added later is covered without being named here.
func foreignKeys(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Closed before the pragma below: the pool is one connection, so a
	// second query while these rows are open would deadlock.
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	graph := map[string][]string{}
	for _, child := range tables {
		parents, err := s.db.Query("PRAGMA foreign_key_list(" + child + ")")
		if err != nil {
			t.Fatal(err)
		}
		for parents.Next() {
			// The pragma's shape is fixed: id, seq, table, from, to,
			// on_update, on_delete, match.
			var id, seq int
			var parent, from, to, onUpdate, onDelete, match sql.NullString
			if err := parents.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				t.Fatal(err)
			}
			graph[child] = append(graph[child], parent.String)
		}
		if err := parents.Err(); err != nil {
			t.Fatal(err)
		}
		if err := parents.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return graph
}

// TestTheConnectionStringStillAsksForFullSynchronous: the durability
// suite cannot see this one.
//
// It reads PRAGMA synchronous from a live connection, and the pinned
// driver already answers 2 with no pragma set at all, so deleting the
// pragma passes every assertion there. Setting it to something else does
// fail, which is the case the suite does cover. What it cannot cover is
// the pragma quietly going away and the guarantee then resting on a
// driver default that a later version is free to change. So the string
// itself is what states it here.
func TestTheConnectionStringStillAsksForFullSynchronous(t *testing.T) {
	if got := DSN("/state/runpool.db"); !strings.Contains(got, "_pragma=synchronous(full)") {
		t.Errorf("the connection string no longer asks for synchronous=FULL: %s\n"+
			"A committed lease transition would survive a crash only for as long as the "+
			"driver keeps defaulting to it, and no suite would notice the change.", got)
	}
}

// TestTheColumnRefusesAReviewerWithNoName: reviewed_by is NULL until a
// person resolves the attempt, beside reviewed_at which already was —
// one event, recorded absent one way. The empty string is refused by the
// column itself, which closes a door the command layer was guarding
// alone: a caller reaching the resolving transaction directly could
// record a resolution with no actor, and the audit trail held a reviewer
// with no name.
func TestTheColumnRefusesAReviewerWithNoName(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	id := seedAttempt(t, s, binding, "msg-anon", "job-anon")
	inTx(t, s, func(tx *Tx) error {
		return tx.HoldForReview(id, ReviewReasonStartOutcomeUnknown)
	})

	err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.ResolveReviewToReady(id, "a reason", "")
	})
	if err == nil {
		t.Fatal("a resolution with no actor was recorded; the audit trail holds a reviewer with no name")
	}

	inTx(t, s, func(tx *Tx) error {
		return tx.ResolveReviewToReady(id, "a reason", "alice")
	})
	var got Attempt
	inTx(t, s, func(tx *Tx) error {
		var err error
		got, err = tx.Get(id)
		return err
	})
	if got.ReviewedBy != "alice" {
		t.Errorf("reviewed_by = %q; want the actor who resolved it", got.ReviewedBy)
	}
}

// TestTheColumnRefusesAnEmptyLaneHolder: a free lane is NULL, and Go and
// SQL each ask in their own language — GC reads LeasedBy == "" off a
// coalesce while the delete guards on leased_by IS NULL. A stored empty
// string would read as free to one and leased to the other: GC would
// call the lane evictable and the delete would refuse it, forever. No
// writer produces one today, which is exactly why the column refuses it
// rather than trusting that to stay true.
func TestTheColumnRefusesAnEmptyLaneHolder(t *testing.T) {
	s := newStore(t)
	err := s.Tx(t.Context(), func(tx *Tx) error {
		project, err := tx.EnsureCacheProject("github.com/acme/app")
		if err != nil {
			return err
		}
		_, err = tx.LeaseCacheLane(project, "gen", "", 4)
		return err
	})
	if err == nil {
		t.Fatal("a lane was leased to an empty holder; freeness now has two spellings")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("the write failed with %v; the column's own constraint is what has to refuse it", err)
	}
}

// TestTheVocabulariesCoverTheirColumns: resolution and review_reason are
// closed in the schema and enumerated in Go, and the two lists are
// written by different hands. A value the machine can produce and the
// column refuses is a write that fails in production at the moment an
// operator is being told what happened; a value the column admits and
// the list omits is one no reader accounts for.
func TestTheVocabulariesCoverTheirColumns(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)

	for _, r := range assignment.AllResolutions {
		id := seedAttempt(t, s, binding, "msg-res-"+string(r), assignment.SourceWorkloadKey("job-res-"+string(r)))
		if err := s.Tx(t.Context(), func(tx *Tx) error {
			return tx.CancelReady(id, r)
		}); err != nil {
			t.Errorf("the column refuses the resolution %q the machine produces: %v", r, err)
		}
	}
	for _, r := range AllReviewReasons {
		id := seedAttempt(t, s, binding, "msg-rr-"+string(r), assignment.SourceWorkloadKey("job-rr-"+string(r)))
		if err := s.Tx(t.Context(), func(tx *Tx) error {
			return tx.HoldForReview(id, r)
		}); err != nil {
			t.Errorf("the column refuses the review reason %q the machine produces: %v", r, err)
		}
	}

	// And the other direction: a word neither list holds is refused by
	// the table rather than stored as an outcome nothing can read.
	bogus := seedAttempt(t, s, binding, "msg-bogus", "job-bogus")
	if err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.CancelReady(bogus, assignment.Resolution("reassigned_by_provider"))
	}); err == nil {
		t.Error("a resolution outside the vocabulary was stored")
	}
}

// TestEveryEventKindIsOneTheColumnAdmits: the trail's fourteen kinds are
// closed by the schema and enumerated in Go, and the two lists are
// written by different hands. A kind the machine can produce and the
// column refuses fails at the write -- in a delivery path, at the moment
// the trail is being written for an operator to read -- and a kind the
// column admits with no constant is one the next writer spells by hand.
func TestEveryEventKindIsOneTheColumnAdmits(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	id := seedAttempt(t, s, binding, "msg-kinds", "job-kinds")

	for _, k := range AllEventKinds {
		if err := s.Tx(t.Context(), func(tx *Tx) error {
			return tx.RecordEvent(id, "idem-"+string(k), k)
		}); err != nil {
			t.Errorf("the column refuses the kind %q the machine produces: %v", k, err)
		}
	}

	// The other direction, against the column itself: the list is not
	// short. Every kind the schema admits has a constant here.
	admitted := checkVocabulary(t, s, "attempt_events", "kind")
	if len(admitted) != len(AllEventKinds) {
		t.Fatalf("the column admits %d kinds and AllEventKinds holds %d: %v",
			len(admitted), len(AllEventKinds), admitted)
	}
	for _, a := range admitted {
		if !slices.Contains(AllEventKinds, EventKind(a)) {
			t.Errorf("the column admits %q and no constant names it", a)
		}
	}
}

// TestTheStateVocabulariesCoverTheirColumns holds the two remaining
// enumerated vocabularies against the columns that close them. Both
// lists live in Go and both sets live in the schema, written by
// different hands: a value the machine can reach and the column refuses
// fails at a transition, mid-lifecycle; a value the column admits with
// no member naming it is one no reader accounts for.
func TestTheStateVocabulariesCoverTheirColumns(t *testing.T) {
	s := newStore(t)

	for name, tc := range map[string]struct {
		table, column string
		declared      []string
	}{
		"lease states": {"capsule_leases", "state         TEXT",
			func() []string {
				var o []string
				for _, v := range AllLeaseStates {
					o = append(o, string(v))
				}
				return o
			}()},
		"resolutions": {"assignment_attempts", "resolution",
			func() []string {
				var o []string
				for _, v := range assignment.AllResolutions {
					o = append(o, string(v))
				}
				return o
			}()},
		"review reasons": {"assignment_attempts", "review_reason",
			func() []string {
				var o []string
				for _, v := range AllReviewReasons {
					o = append(o, string(v))
				}
				return o
			}()},
		"execution evidence": {"assignment_attempts", "execution_evidence",
			func() []string {
				var o []string
				for _, v := range AllEvidence {
					o = append(o, string(v))
				}
				return o
			}()},
	} {
		t.Run(name, func(t *testing.T) {
			cols := checkVocabulary(t, s, tc.table, tc.column)
			slices.Sort(cols)
			declared := slices.Clone(tc.declared)
			slices.Sort(declared)
			if !slices.Equal(cols, declared) {
				t.Errorf("the column admits %v\nand Go enumerates %v\n"+
					"a value in one list and not the other is one that fails at a write "+
					"or one nothing accounts for", cols, declared)
			}
		})
	}
}

// checkVocabulary reads the values a column's own CHECK admits, from the
// live schema rather than a copy of it.
//
// Two details are the whole of why it is a function. The window starts
// at the CHECK and not at the column, because a DEFAULT sitting beside
// it is a quoted literal too and counting it made a vocabulary look like
// it held a value twice. And the class is "anything but a quote", not
// "lowercase and underscore": the narrow class silently dropped any
// value carrying a digit or a capital, so a word added to the column and
// not to Go -- the exact drift this reads for -- would have left the two
// sets equal and passed.
//
// Every way of mis-reading a CHECK this does not expect over-collects or
// under-collects, so it fails. A missing CHECK is fatal rather than
// silently harvesting the columns below it.
func checkVocabulary(t *testing.T, s *Store, table, columnAnchor string) []string {
	t.Helper()
	var ddl string
	inTx(t, s, func(tx *Tx) error {
		return tx.tx.QueryRow(
			`SELECT sql FROM sqlite_schema WHERE name = ?`, table).Scan(&ddl)
	})
	i := strings.Index(ddl, columnAnchor)
	if i < 0 {
		t.Fatalf("%s: no column matching %q in the stored schema; the anchor this reads "+
			"by has moved", table, columnAnchor)
	}
	rest := ddl[i:]
	c := strings.Index(rest, "CHECK")
	if c < 0 {
		t.Fatalf("%s.%s has no CHECK; this reads a closed vocabulary and the column no "+
			"longer closes one", table, columnAnchor)
	}
	rest = rest[c:]
	end := strings.Index(rest, "))")
	if end < 0 {
		t.Fatalf("the CHECK on %s.%s is not the shape this reads", table, columnAnchor)
	}
	var out []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(rest[:end], -1) {
		out = append(out, m[1])
	}
	return out
}

// TestTheEstablishingObservationsAreExactlyWhatTheColumnAdmits.
// start_observation holds what a serving measured about whether the
// workload began, and it admits four of the vocabulary's seven values.
// Which four is decided in another package, by
// ExecutionObservation.Establishes, and the only thing between a value
// the column refuses and a write that fails is one `if` in the lease
// manager.
//
// A fifth establishing observation added there passes that guard, the
// write is attempted, and the column refuses it -- inside the cleanup
// that ends a serving, which then quarantines the lease it could not
// finish. The subset is a correspondence between two packages and a
// schema, and nothing held it.
func TestTheEstablishingObservationsAreExactlyWhatTheColumnAdmits(t *testing.T) {
	admitted := checkVocabulary(t, newStore(t), "capsule_leases", "start_observation")
	slices.Sort(admitted)

	var establishes []string
	for _, o := range assignment.AllExecutionObservations {
		if o.Establishes() {
			establishes = append(establishes, string(o))
		}
	}
	slices.Sort(establishes)

	if !slices.Equal(admitted, establishes) {
		t.Errorf("the column admits %v\nand Establishes() answers for %v\n"+
			"a value that establishes and the column refuses fails the write that ends a "+
			"serving; a value the column admits and nothing establishes is one no pass writes",
			admitted, establishes)
	}
}

// TestEveryEvidenceAdvanceReachesTheTrail: evidence is a high-water mark
// and says only where an attempt got, never when. An attempt held
// because a start was authorized and its runtime could not be observed
// therefore showed an operator a trail that jumped from lease_attached
// to the hold, with the authorization that caused it durable nowhere but
// a log that rotates — and that authorization is the at-most-once line,
// the one fact a person resolving the hold is deciding about.
func TestEveryEvidenceAdvanceReachesTheTrail(t *testing.T) {
	s := newStore(t)
	binding := seedBinding(t, s)
	id := seedAttempt(t, s, binding, "msg-trail", "job-trail")

	for _, e := range AllEvidence[1:] {
		if err := s.Tx(t.Context(), func(tx *Tx) error {
			return tx.RecordEvidence(id, e)
		}); err != nil {
			t.Fatalf("recording %s: %v", e, err)
		}
	}

	var kinds []EventKind
	inTx(t, s, func(tx *Tx) error {
		events, err := tx.Events(id)
		if err != nil {
			return err
		}
		for _, ev := range events {
			kinds = append(kinds, EventKind(ev.Kind))
		}
		return nil
	})
	for _, e := range AllEvidence[1:] {
		want := eventKindOf(e)
		if want == "" {
			t.Errorf("the advance to %s names no trail entry", e)
			continue
		}
		if !slices.Contains(kinds, want) {
			t.Errorf("advancing to %s wrote no %s into the trail: %v", e, want, kinds)
		}
		if !slices.Contains(AllEventKinds, want) {
			t.Errorf("%s is not a kind the column admits", want)
		}
	}

	// Re-observing a fact is idempotent, and so is its trail entry.
	before := len(kinds)
	if err := s.Tx(t.Context(), func(tx *Tx) error {
		return tx.RecordEvidence(id, EvidenceExitObserved)
	}); err != nil {
		t.Fatal(err)
	}
	var after int
	inTx(t, s, func(tx *Tx) error {
		events, err := tx.Events(id)
		after = len(events)
		return err
	})
	if after != before {
		t.Errorf("re-observing the same evidence wrote %d more entries; a repeated "+
			"observation is one fact, not two", after-before)
	}
}
