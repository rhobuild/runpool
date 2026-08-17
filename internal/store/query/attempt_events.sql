-- Attempt lifecycle events: append-only, idempotent per
-- (attempt, idempotency key), so a redelivered message cannot double
-- history. Zero rows from the insert means the event already exists,
-- which the repository reports as idempotent success after verifying
-- the existing event matches.

-- name: InsertAttemptEvent :execrows
INSERT INTO attempt_events (attempt_id, idempotency_key, kind, detail_json)
VALUES (@attempt_id, @idempotency_key, @kind, @detail_json)
ON CONFLICT (attempt_id, idempotency_key) DO NOTHING;

-- name: GetAttemptEventByKey :one
SELECT id, attempt_id, idempotency_key, kind, detail_json, created_at
FROM attempt_events
WHERE attempt_id = @attempt_id
  AND idempotency_key = @idempotency_key;

-- name: ListAttemptEvents :many
SELECT id, attempt_id, idempotency_key, kind, detail_json, created_at
FROM attempt_events
WHERE attempt_id = @attempt_id
ORDER BY id;
