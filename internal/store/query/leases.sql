-- Capsule leases are the durable ownership record for one serving.

-- name: InsertLease :exec
INSERT INTO capsule_leases (id, binding_id, attempt_id, tier_id, state)
VALUES (@id, @binding_id, @attempt_id, @tier_id, @state);

-- name: TransitionLease :execrows
UPDATE capsule_leases
SET state = @next, updated_at = unixepoch()
WHERE id = @id AND state = @current;

-- name: SetLeaseRuntimeName :execrows
UPDATE capsule_leases
SET runtime_name = @runtime_name, updated_at = unixepoch()
WHERE id = @id;

-- name: SetLeaseStartObservation :execrows
UPDATE capsule_leases
SET start_observation = @start_observation, updated_at = unixepoch()
WHERE id = @id;

-- name: GetLease :one
SELECT id, binding_id, attempt_id, tier_id, state, runtime_name,
       start_observation, created_at, updated_at
FROM capsule_leases
WHERE id = @id;

-- name: GetAttemptIDByRuntimeName :one
SELECT attempt_id FROM capsule_leases
WHERE runtime_name = @runtime_name
ORDER BY rowid DESC
LIMIT 1;

-- name: GetNewestLeaseByAttempt :one
SELECT id, binding_id, attempt_id, tier_id, state, runtime_name,
       start_observation, created_at, updated_at
FROM capsule_leases
WHERE attempt_id = @attempt_id
ORDER BY rowid DESC
LIMIT 1;

-- name: CountReleasedLeases :one
SELECT count(*) FROM capsule_leases WHERE state = 'released';

-- name: ListRecentReleasedLeases :many
SELECT id, binding_id, attempt_id, tier_id, state, runtime_name,
       start_observation, created_at, updated_at
FROM capsule_leases
WHERE state = 'released'
ORDER BY updated_at DESC, id DESC
LIMIT @row_limit;

-- name: CountPrunableReleasedLeases :one
SELECT count(*) FROM (
  SELECT lease.id FROM capsule_leases AS lease
  WHERE lease.state = 'released' AND lease.updated_at < @before
    AND lease.attempt_id NOT IN (
      SELECT attempt.id FROM assignment_attempts AS attempt
      WHERE attempt.state IN ('ready', 'leased', 'preparing', 'prepared',
                      'starting', 'running', 'manual_review'))
    AND lease.id NOT IN (SELECT intent.lease_id FROM resource_intents AS intent)
  ORDER BY lease.updated_at
  LIMIT @row_limit
);

-- name: PruneReleasedLeases :execrows
DELETE FROM capsule_leases WHERE capsule_leases.id IN (
  SELECT lease.id FROM capsule_leases AS lease
  WHERE lease.state = 'released' AND lease.updated_at < @before
    AND lease.attempt_id NOT IN (
      SELECT attempt.id FROM assignment_attempts AS attempt
      WHERE attempt.state IN ('ready', 'leased', 'preparing', 'prepared',
                      'starting', 'running', 'manual_review'))
    AND lease.id NOT IN (SELECT intent.lease_id FROM resource_intents AS intent)
  ORDER BY lease.updated_at
  LIMIT @row_limit
);

-- name: CountLeasesByAttempt :one
SELECT count(*) FROM capsule_leases WHERE attempt_id = @attempt_id;

-- name: PurgeResolvedLease :execrows
DELETE FROM capsule_leases
WHERE capsule_leases.id = @id AND capsule_leases.attempt_id NOT IN (
  SELECT attempt.id FROM assignment_attempts AS attempt
  WHERE attempt.state IN ('ready', 'leased', 'preparing', 'prepared',
                  'starting', 'running', 'manual_review')
);
