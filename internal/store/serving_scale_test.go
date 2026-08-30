package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
)

const readyBacklogAcceptance = 100_000

func TestReadyAttemptBatchBoundsALargeBacklogAndPreservesFIFO(t *testing.T) {
	store := newStore(t)
	bindingID := seedReadyBacklog(t, store, readyBacklogAcceptance)

	var single []Attempt
	inTx(t, store, func(tx *Tx) error {
		var err error
		single, err = tx.ReadyAttemptBatch(bindingID, 1)
		return err
	})
	if len(single) != 1 {
		t.Fatalf("one available credit materialized %d attempts from a %d-row backlog; want 1", len(single), readyBacklogAcceptance)
	}

	var batch []Attempt
	inTx(t, store, func(tx *Tx) error {
		var err error
		batch, err = tx.ReadyAttemptBatch(bindingID, 2)
		return err
	})
	if len(batch) != 2 {
		t.Fatalf("batch materialized %d attempts; want 2", len(batch))
	}
	if batch[0].ID != "attempt-000000" || batch[1].ID != "attempt-000001" {
		t.Fatalf("batch order = [%s %s]; want the two oldest ids", batch[0].ID, batch[1].ID)
	}
	inTx(t, store, func(tx *Tx) error {
		count, err := tx.CountReadyAttempts(bindingID)
		if err == nil && count != readyBacklogAcceptance {
			t.Errorf("ready count = %d; want %d", count, readyBacklogAcceptance)
		}
		return err
	})

	err := store.Tx(t.Context(), func(tx *Tx) error {
		_, err := tx.ReadyAttemptBatch(bindingID, 0)
		return err
	})
	if err == nil {
		t.Fatal("a zero batch size was accepted; non-positive limits must not reach SQLite")
	}
}

func TestConcurrentSchedulersCreateOneLease(t *testing.T) {
	store := newStore(t)
	bindingID := seedBinding(t, store)
	attemptID := seedAttempt(t, store, bindingID, "delivery-race", "workload-race")

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- store.Tx(context.Background(), func(tx *Tx) error {
				_, err := tx.LeaseAttempt(attemptID, bindingID, "standard")
				return err
			})
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	winners, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("scheduler returned %v; want success or ErrConflict", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("scheduler outcomes: winners=%d conflicts=%d; want one of each", winners, conflicts)
	}
	inTx(t, store, func(tx *Tx) error {
		var count int
		if err := tx.tx.QueryRow(`SELECT COUNT(*) FROM capsule_leases WHERE attempt_id = ?`, attemptID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("attempt has %d leases after the race; want 1", count)
		}
		return nil
	})
}

func BenchmarkReadyAttemptBatchWithLargeBacklog(b *testing.B) {
	// Acceptance profile: a 100,000-row ready backlog and one available
	// credit. The row-count assertion above is the portable memory gate;
	// this benchmark records the platform-specific latency and allocations.
	store, err := Open(b.TempDir(), DefaultRetryBudget)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Error(err)
		}
	})
	bindingID := seedReadyBacklog(b, store, readyBacklogAcceptance)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var batch []Attempt
		if err := store.Tx(context.Background(), func(tx *Tx) error {
			var err error
			batch, err = tx.ReadyAttemptBatch(bindingID, 1)
			return err
		}); err != nil {
			b.Fatal(err)
		}
		if len(batch) != 1 {
			b.Fatalf("batch size = %d; want 1", len(batch))
		}
	}
}

func seedReadyBacklog(tb testing.TB, store *Store, count int) assignment.BindingID {
	tb.Helper()
	var bindingID assignment.BindingID
	if err := store.Tx(context.Background(), func(tx *Tx) error {
		var err error
		bindingID, err = tx.EnsureBinding("scale", "github_actions",
			"v1|repository|https://github.com/acme/scale||runpool-standard")
		if err != nil {
			return err
		}
		result, err := tx.tx.Exec(`
			INSERT INTO broker_deliveries (binding_id, source_delivery_key, payload_sha256)
			VALUES (?, 'large-backlog', zeroblob(32))`, bindingID)
		if err != nil {
			return err
		}
		deliveryID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		statement, err := tx.tx.Prepare(`
			INSERT INTO assignment_attempts
				(id, delivery_id, binding_id, source_workload_key, tenant_key, project_key, received_at)
			VALUES (?, ?, ?, ?, 'acme', 'scale', 1)`)
		if err != nil {
			return err
		}
		for index := range count {
			id := fmt.Sprintf("attempt-%06d", index)
			if _, err := statement.Exec(id, deliveryID, bindingID, "workload-"+id); err != nil {
				_ = statement.Close()
				return err
			}
		}
		return statement.Close()
	}); err != nil {
		tb.Fatal(err)
	}
	return bindingID
}
