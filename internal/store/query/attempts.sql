-- Assignment attempts: the local execution lifecycle of one workload
-- under one delivery, and the sole owner of what was observed about it.
-- Every state change is compare-and-swap: zero rows affected means the
-- row moved, and the caller re-reads instead of forcing the write. ASCII
-- only in these files - sqlc v1.31.1's sqlite lexer loses the comment
-- boundary on multibyte characters and reports phantom parse errors
-- several statements later.
--
-- Nothing here names a lease. The link lives on capsule_leases, so
-- purging a finished lease takes the link with it and no settled attempt
-- can pin a runtime row forever.

-- name: InsertAttempt :execrows
INSERT INTO assignment_attempts
	(id, delivery_id, binding_id, source_workload_key, tenant_key, project_key)
VALUES
	(@id, @delivery_id, @binding_id, @source_workload_key, @tenant_key, @project_key)
ON CONFLICT (delivery_id, source_workload_key) DO NOTHING;

-- name: GetAttempt :one
SELECT id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
       state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at
FROM assignment_attempts
WHERE id = @id;

-- name: GetAttemptByDeliveryAndWorkload :one
SELECT id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
       state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at
FROM assignment_attempts
WHERE delivery_id = @delivery_id
  AND source_workload_key = @source_workload_key;

-- name: GetOpenAttemptByWorkload :one
SELECT id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
       state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at
FROM assignment_attempts
WHERE binding_id = @binding_id
  AND source_workload_key = @source_workload_key
  AND state IN ('ready', 'leased', 'preparing', 'prepared',
                'starting', 'running', 'manual_review');

-- name: ListReadyAttempts :many
SELECT id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
       state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at
FROM assignment_attempts
WHERE binding_id = @binding_id AND state = 'ready'
ORDER BY received_at, id;

-- name: ListManualReviewAttempts :many
SELECT id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
       state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at
FROM assignment_attempts
WHERE state = 'manual_review'
ORDER BY received_at, id;

-- name: GetAttemptByLease :one
SELECT a.id, a.delivery_id, a.binding_id, a.source_workload_key, a.tenant_key,
       a.project_key, a.state, a.execution_evidence, a.resolution,
       a.review_reason, a.reviewed_at, a.reviewed_by, a.received_at, a.settled_at
FROM assignment_attempts a
JOIN capsule_leases l ON l.attempt_id = a.id
WHERE l.id = @lease_id;

-- name: ListAttemptsByDelivery :many
SELECT id, delivery_id, binding_id, source_workload_key, tenant_key, project_key,
       state, execution_evidence, resolution, review_reason, reviewed_at,
       reviewed_by, received_at, settled_at
FROM assignment_attempts
WHERE delivery_id = @delivery_id
ORDER BY received_at, id;

-- name: ClaimReadyAttempt :execrows
-- Claiming is the compare-and-swap that decides who serves a workload:
-- exactly one caller moves it out of ready, and the lease row committed
-- alongside it in the same transaction carries the link back.
UPDATE assignment_attempts
SET state = 'leased'
WHERE id = @attempt_id AND state = 'ready';

-- name: TransitionAttempt :execrows
UPDATE assignment_attempts
SET state = @next
WHERE id = @attempt_id AND state = @current;

-- name: RecordAttemptEvidence :execrows
-- Evidence is monotonic and compare-and-swap on the value the caller
-- read. If another writer moved it in between, nothing is written and
-- the caller re-reads: an observation that was made cannot be unmade by
-- a slower writer.
UPDATE assignment_attempts
SET execution_evidence = @next
WHERE id = @attempt_id AND execution_evidence = @current;

-- name: RequeueAttempt :execrows
-- Only stages that provably started nothing may return to ready:
-- leased, preparing and prepared all precede the start authorization.
-- From starting onward at-most-once rules, and requeue must refuse.
--
-- The evidence resets like the proof-carrying requeues' does, and for
-- the same reason: the serving is over and the next one starts from
-- nothing. The authorization can be present here - recording it commits
-- before the best-effort walk to starting - and left behind it makes the
-- next serving's first honest observation a write that moves backwards.
UPDATE assignment_attempts
SET state = 'ready', execution_evidence = 'not_started'
WHERE id = @attempt_id AND state IN ('leased', 'preparing', 'prepared');

-- name: RequeueProvenInertAttempt :execrows
-- The one legal requeue past the start authorization: the daemon proved
-- the start never took effect (the runtime is still in its created
-- state), so the at-most-once rule has nothing to protect.
--
-- The evidence goes back with it. It is monotonic within one serving, and
-- the proof this requeue carries is exactly what ends that serving: the
-- capsule is destroyed and the next one starts from nothing, so leaving
-- the authorization behind makes its first honest observation - the
-- runtime being prepared - a write that moves backwards, and every retry
-- of this shape ends in review after burning a lease. What each serving
-- observed is kept in attempt_events either way.
UPDATE assignment_attempts
SET state = 'ready', execution_evidence = 'not_started'
WHERE id = @attempt_id AND state = 'starting';

-- name: CancelReadyAttempt :execrows
-- A remote cancellation may close an attempt that is not yet executing;
-- anything past ready is running work, whose cancellation is a drain
-- concern, not a row update.
UPDATE assignment_attempts
SET state = 'canceled', resolution = @resolution, settled_at = unixepoch()
WHERE id = @attempt_id AND state = 'ready';

-- name: SettleAttempt :execrows
UPDATE assignment_attempts
SET state = 'settled', resolution = @resolution, settled_at = unixepoch()
WHERE id = @attempt_id AND state = @current;

-- name: SupersedeAttempt :execrows
UPDATE assignment_attempts
SET state = 'superseded', resolution = @resolution, settled_at = unixepoch()
WHERE id = @attempt_id
  AND state IN ('ready', 'leased', 'prepared');

-- name: MarkAttemptManualReview :execrows
UPDATE assignment_attempts
SET state = 'manual_review', review_reason = @review_reason
WHERE id = @attempt_id
  AND state IN ('ready', 'leased', 'preparing', 'prepared', 'starting', 'running');

-- name: ResolveManualReviewToReady :execrows
-- An operator who resolves to ready has established what review could
-- not, and that verdict ends the serving being reviewed. The evidence
-- resets with it for the same reason RequeueProvenInertAttempt's does:
-- the next serving starts from nothing, and what this one observed
-- remains in attempt_events.
UPDATE assignment_attempts
SET state = 'ready', execution_evidence = 'not_started', review_reason = '',
    resolution = @resolution, reviewed_at = unixepoch(), reviewed_by = @reviewed_by
WHERE id = @attempt_id AND state = 'manual_review';

-- name: ResolveManualReviewToSettled :execrows
UPDATE assignment_attempts
SET state = 'settled', resolution = @resolution,
    reviewed_at = unixepoch(), reviewed_by = @reviewed_by,
    settled_at = unixepoch()
WHERE id = @attempt_id AND state = 'manual_review';

-- name: CountReadyAttempts :one
-- The depth of a binding's queue. A ready attempt is served in age order
-- as soon as a lane frees, so a count that stays high is the operator's
-- signal that no lane is freeing.
SELECT COUNT(*) FROM assignment_attempts
WHERE binding_id = @binding_id AND state = 'ready';
