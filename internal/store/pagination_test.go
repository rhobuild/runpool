package store

import (
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
)

func TestAttemptPagesUseTheirGlobalFIFOIndexes(t *testing.T) {
	state := newStore(t)
	queries := []struct {
		name  string
		query string
		index string
	}{
		{
			name: "manual review", index: "attempts_manual_review",
			query: `EXPLAIN QUERY PLAN
				SELECT id FROM assignment_attempts
				WHERE state = 'manual_review'
				  AND (received_at > ? OR (received_at = ? AND id > ?))
				ORDER BY received_at, id LIMIT ?`,
		},
		{
			name: "ready", index: "attempts_ready_global",
			query: `EXPLAIN QUERY PLAN
				SELECT id FROM assignment_attempts
				WHERE state = 'ready'
				  AND (received_at > ? OR (received_at = ? AND id > ?))
				ORDER BY received_at, id LIMIT ?`,
		},
	}
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, state, func(tx *Tx) error {
				rows, err := tx.tx.Query(tc.query, 0, 0, "", 51)
				if err != nil {
					return err
				}
				defer rows.Close()
				var plan []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						return err
					}
					plan = append(plan, detail)
				}
				if err := rows.Err(); err != nil {
					return err
				}
				detail := strings.Join(plan, "; ")
				if !strings.Contains(detail, tc.index) {
					t.Errorf("query plan %q does not use %s", detail, tc.index)
				}
				if strings.Contains(detail, "TEMP B-TREE") {
					t.Errorf("query plan sorts through a temporary B-tree: %q", detail)
				}
				return nil
			})
		})
	}
}

func TestAttemptPagesUseStableFIFOKeysAndReportTheWholeQueue(t *testing.T) {
	state := newStore(t)
	bindingID := seedAttemptPageRows(t, state)

	var first AttemptPage
	inTx(t, state, func(tx *Tx) error {
		var err error
		first, err = tx.ManualReviewAttemptPage(nil, 2)
		return err
	})
	assertAttemptPage(t, first, 4, true, "review-a", "review-b")
	if got := first.Next.ReceivedAt; !got.Equal(time.Unix(10, 0).UTC()) {
		t.Fatalf("first page cursor time = %s; want %s", got, time.Unix(10, 0).UTC())
	}

	var second AttemptPage
	inTx(t, state, func(tx *Tx) error {
		var err error
		second, err = tx.ManualReviewAttemptPage(first.Next, 2)
		return err
	})
	assertAttemptPage(t, second, 4, false, "review-c", "review-d")

	var ready AttemptPage
	inTx(t, state, func(tx *Tx) error {
		var err error
		ready, err = tx.ReadyAttemptPage(nil, 2)
		return err
	})
	assertAttemptPage(t, ready, 3, true, "ready-a", "ready-b")
	if ready.Attempts[0].BindingID != bindingID {
		t.Fatalf("ready attempt binding = %d; want %d", ready.Attempts[0].BindingID, bindingID)
	}
}

func TestAttemptPagesRejectUnboundedAndMalformedRequests(t *testing.T) {
	state := newStore(t)
	cases := []struct {
		name   string
		cursor *AttemptCursor
		size   int
	}{
		{name: "zero page", size: 0},
		{name: "oversized page", size: MaxAttemptPageSize + 1},
		{name: "cursor without id", size: 1, cursor: &AttemptCursor{ReceivedAt: time.Unix(1, 0)}},
		{name: "cursor before epoch", size: 1, cursor: &AttemptCursor{ReceivedAt: time.Unix(-1, 0), ID: "attempt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := state.Tx(t.Context(), func(tx *Tx) error {
				_, err := tx.ManualReviewAttemptPage(tc.cursor, tc.size)
				return err
			})
			if err == nil {
				t.Fatal("invalid page request was accepted")
			}
		})
	}
}

func assertAttemptPage(t *testing.T, page AttemptPage, total int64, hasNext bool, ids ...assignment.AttemptID) {
	t.Helper()
	if page.Total != total {
		t.Errorf("page total = %d; want %d", page.Total, total)
	}
	if (page.Next != nil) != hasNext {
		t.Errorf("page next present = %t; want %t", page.Next != nil, hasNext)
	}
	if len(page.Attempts) != len(ids) {
		t.Fatalf("page has %d attempts; want %d", len(page.Attempts), len(ids))
	}
	for index, id := range ids {
		if page.Attempts[index].ID != id {
			t.Errorf("attempt %d = %s; want %s", index, page.Attempts[index].ID, id)
		}
	}
}

func seedAttemptPageRows(t *testing.T, state *Store) assignment.BindingID {
	t.Helper()
	bindingID := seedBinding(t, state)
	inTx(t, state, func(tx *Tx) error {
		result, err := tx.tx.Exec(`
			INSERT INTO broker_deliveries (binding_id, source_delivery_key, payload_sha256)
			VALUES (?, 'pagination', zeroblob(32))`, bindingID)
		if err != nil {
			return err
		}
		deliveryID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		rows := []struct {
			id         string
			state      AttemptState
			receivedAt int64
		}{
			{"review-d", AttemptManualReview, 12},
			{"review-b", AttemptManualReview, 10},
			{"review-c", AttemptManualReview, 11},
			{"review-a", AttemptManualReview, 10},
			{"ready-c", AttemptReady, 22},
			{"ready-a", AttemptReady, 20},
			{"ready-b", AttemptReady, 21},
		}
		for _, row := range rows {
			if _, err := tx.tx.Exec(`
				INSERT INTO assignment_attempts
					(id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
					 state, review_reason, received_at)
				VALUES (?, ?, ?, ?, 'tenant', 'project', ?, 'start_outcome_unknown', ?)`,
				row.id, deliveryID, bindingID, "workload-"+row.id, row.state, row.receivedAt); err != nil {
				return err
			}
		}
		return nil
	})
	return bindingID
}
