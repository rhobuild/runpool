-- Provider bindings: the neutral identity a delivery belongs to.
-- Parameters are named (@x) throughout: positional arguments invite the
-- column-order bugs this generation step exists to eliminate.

-- name: InsertProviderBinding :one
INSERT INTO provider_bindings (target_id, provider_kind, source_binding_key)
VALUES (@target_id, @provider_kind, @source_binding_key)
ON CONFLICT (provider_kind, source_binding_key) DO UPDATE SET target_id = excluded.target_id
RETURNING id, target_id, provider_kind, source_binding_key, created_at;

-- name: RecordProviderContact :exec
-- A success clears the failure: what is reported is the current state of
-- the binding's reach, not a log of everything that ever went wrong.
INSERT INTO provider_binding_contact (binding_id, last_contact_at_ms, last_error, last_error_at_ms)
VALUES (@binding_id, @at, '', 0)
ON CONFLICT (binding_id) DO UPDATE
SET last_contact_at_ms = excluded.last_contact_at_ms, last_error = '', last_error_at_ms = 0;

-- name: RecordProviderFailure :exec
-- The last contact is left alone: how long a binding has been unable to
-- reach its provider is the question a failure raises.
INSERT INTO provider_binding_contact (binding_id, last_contact_at_ms, last_error, last_error_at_ms)
VALUES (@binding_id, 0, @last_error, @at)
ON CONFLICT (binding_id) DO UPDATE
SET last_error = excluded.last_error, last_error_at_ms = excluded.last_error_at_ms;

-- name: ListProviderBindingsWithContact :many
-- The reach travels with the binding in one query: a list plus a lookup
-- joined in Go is two reads a write can straddle, and the join is the
-- database's job. A binding that has never run reports zeroes, which is
-- not the same as failing: the first has nothing to say, the second has
-- a reason.
SELECT b.id, b.target_id, b.provider_kind, b.source_binding_key, b.created_at,
       coalesce(c.last_contact_at_ms, 0) AS last_contact_at_ms,
       coalesce(c.last_error, '')        AS last_error,
       coalesce(c.last_error_at_ms, 0)   AS last_error_at_ms
FROM provider_bindings b
LEFT JOIN provider_binding_contact c ON c.binding_id = b.id
ORDER BY b.target_id, b.source_binding_key;
