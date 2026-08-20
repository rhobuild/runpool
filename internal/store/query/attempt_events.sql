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
-- What the conflict clause on the insert above protects against is a
-- redelivered message: those events key by delivery id, which is the
-- identity the outside world repeats. A hold or a resolve has no such
-- identity. The command runs once, and a second run finds the
-- compare-and-swap refusing, so a fixed key protected against nothing
-- while dropping every later decision to do it.
--
-- This one carries no such clause, because it has no reachable conflict.
-- The count only grows: the sole delete of these rows drops the attempts
-- with them, and a row written under the old fixed key shifts the
-- numbering without ever occupying a suffixed one. Two writes inside one
-- transaction do not collide either, because SQLite shows an uncommitted
-- insert to the statements after it, so the second counts the first and
-- takes the next number.
--
-- Leaving the clause on would mean that a duplicate nobody can construct
-- would be dropped in silence, with no error, from an append-only audit
-- log. If one is ever constructed, saying so is the only useful answer.
INSERT INTO attempt_events (attempt_id, idempotency_key, kind, detail_json)
SELECT @attempt_id,
       @kind || ':' || (SELECT count(*) FROM attempt_events prior
                        WHERE prior.attempt_id = @attempt_id AND prior.kind = @kind),
       @kind, @detail_json;

-- name: ListAttemptEvents :many
SELECT id, attempt_id, idempotency_key, kind, detail_json, created_at
FROM attempt_events
WHERE attempt_id = @attempt_id
ORDER BY id;
