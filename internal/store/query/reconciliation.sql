-- Invariant queries the reconciler runs at startup and periodically.
-- Each one should return nothing; a row is state that a crash window
-- left behind, and it is repaired only when the evidence determines a
-- single resolution.

-- name: ListAttemptsAttachedToReleasedLeases :many
-- The crash window between releasing a lease and disposing of its
-- attempt. Every row is work nothing else will ever look at.
--
-- An attempt holds one lease per serving, so a released one proves
-- nothing on its own: a retry in flight has its predecessor released and
-- its own lease live. The live lease is what is looking at it, and
-- repairing that is how a restart during a retry tears down the serving
-- it just started.
SELECT a.id, a.delivery_id, a.binding_id, a.source_workload_key, a.tenant_key,
       a.project_key, a.state, a.execution_evidence, a.resolution,
       a.review_reason, a.reviewed_at, a.reviewed_by, a.received_at, a.settled_at
FROM assignment_attempts a
WHERE a.state IN ('leased', 'preparing', 'prepared', 'starting', 'running')
  AND EXISTS (SELECT 1 FROM capsule_leases r
              WHERE r.attempt_id = a.id AND r.state = 'released')
  AND NOT EXISTS (SELECT 1 FROM capsule_leases v
                  WHERE v.attempt_id = a.id AND v.state <> 'released')
ORDER BY a.received_at, a.id;

-- name: CountOpenAttempts :one
SELECT count(*) FROM assignment_attempts
WHERE binding_id = @binding_id
  AND state IN ('ready', 'leased', 'preparing', 'prepared',
                'starting', 'running', 'manual_review');
