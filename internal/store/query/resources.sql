-- Durable intents around external container-engine effects.

-- name: InsertResourceIntent :one
INSERT INTO resource_intents (lease_id, kind, role, name)
VALUES (@lease_id, @kind, @role, @name)
RETURNING id;

-- name: MarkResourceCreating :execrows
UPDATE resource_intents
SET state = @creating, updated_at = unixepoch()
WHERE id = @id AND state = @planned;

-- name: MarkResourcePresent :execrows
UPDATE resource_intents
SET state = @present, docker_id = @docker_id, updated_at = unixepoch()
WHERE id = @id AND state IN (@planned, @creating);

-- name: MarkResourcesForCleanup :exec
UPDATE resource_intents
SET state = @cleanup_pending, updated_at = unixepoch()
WHERE lease_id = @lease_id AND state IN (@planned, @creating, @present);

-- name: MarkResourceDeleting :execrows
UPDATE resource_intents
SET state = @deleting, updated_at = unixepoch()
WHERE id = @id AND state IN (@cleanup_pending, @deleting);

-- name: DeleteResourceIntent :execrows
DELETE FROM resource_intents WHERE id = @id;

-- name: RecordResourceError :execrows
UPDATE resource_intents
SET retries = retries + 1, last_error = @last_error,
    not_before = @not_before, updated_at = unixepoch()
WHERE id = @id;

-- name: ListResourcesByLease :many
SELECT id, lease_id, kind, role, name, docker_id, state, retries,
       last_error, not_before, created_at, updated_at
FROM resource_intents
WHERE lease_id = @lease_id
ORDER BY id;

-- name: ListPendingResourceRemovals :many
SELECT id, lease_id, kind, role, name, docker_id, state, retries,
       last_error, not_before, created_at, updated_at
FROM resource_intents
WHERE state IN (@cleanup_pending, @deleting) AND not_before <= @now
ORDER BY not_before, id
LIMIT @row_limit;
