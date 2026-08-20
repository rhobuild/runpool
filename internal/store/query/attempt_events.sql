-- Attempt lifecycle events: append-only, idempotent per
-- (attempt, idempotency key), so a redelivered message cannot double
-- history. Zero rows from an insert means the event already exists under
-- that key, which the repository reports as success: the key is what
-- makes two writes the same event, so nothing is read back to confirm
-- it.

-- name: InsertAttemptEvent :execrows
-- For a decision that happens at most once per attempt, or one whose key
-- already carries what makes it distinct -- a lease id, a delivery id.
INSERT INTO attempt_events (attempt_id, idempotency_key, kind, detail_json)
VALUES (@attempt_id, @idempotency_key, @kind, @detail_json)
ON CONFLICT (attempt_id, idempotency_key) DO NOTHING;

-- name: InsertSequencedAttemptEvent :execrows
-- For a decision that can be made about one attempt more than once: held
-- for review, resolved by an operator, served again, held again.
--
-- A fixed key makes every occurrence after the first a replay of it, and
-- the conflict clause then drops the actor, the reason and the decision
-- without a word. The attempt row keeps only the latest of each, so what
-- is lost is precisely the history -- who decided what, and why, the
-- time before.
--
-- The key carries how many of this kind the attempt already has.
--
-- What the conflict clause protects in this table is a redelivered
-- message: those events key by delivery id, which is the identity the
-- outside world repeats. A hold or a resolve has no such identity. The
-- command runs once, and a second run finds the compare-and-swap
-- refusing, so a fixed key was protecting against nothing and dropping
-- every later decision to do it.
--
-- The clause still means something here. Two writes of this kind inside
-- one transaction would compute the same count and the second would be
-- dropped, so the invariant a caller owes is one such decision per
-- transaction, which is what each disposition does.
INSERT INTO attempt_events (attempt_id, idempotency_key, kind, detail_json)
SELECT @attempt_id,
       @kind || ':' || (SELECT count(*) FROM attempt_events prior
                        WHERE prior.attempt_id = @attempt_id AND prior.kind = @kind),
       @kind, @detail_json
WHERE true
ON CONFLICT (attempt_id, idempotency_key) DO NOTHING;

-- name: ListAttemptEvents :many
SELECT id, attempt_id, idempotency_key, kind, detail_json, created_at
FROM attempt_events
WHERE attempt_id = @attempt_id
ORDER BY id;
