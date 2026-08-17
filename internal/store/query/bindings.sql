-- Provider bindings: the neutral identity a delivery belongs to.
-- Parameters are named (@x) throughout: positional arguments invite the
-- column-order bugs this generation step exists to eliminate.

-- name: InsertProviderBinding :one
INSERT INTO provider_bindings (target_id, provider_kind, source_binding_key)
VALUES (@target_id, @provider_kind, @source_binding_key)
ON CONFLICT (provider_kind, source_binding_key) DO UPDATE SET target_id = excluded.target_id
RETURNING id, target_id, provider_kind, source_binding_key, desired_state, created_at;

-- name: GetProviderBinding :one
SELECT id, target_id, provider_kind, source_binding_key, desired_state, created_at
FROM provider_bindings
WHERE provider_kind = @provider_kind
  AND source_binding_key = @source_binding_key;

-- name: ListProviderBindings :many
SELECT id, target_id, provider_kind, source_binding_key, desired_state, created_at
FROM provider_bindings
ORDER BY target_id, source_binding_key;

-- name: SetBindingDesiredState :execrows
UPDATE provider_bindings
SET desired_state = @desired_state
WHERE id = @binding_id;

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

-- name: ListProviderBindingContact :many
SELECT binding_id, last_contact_at_ms, last_error, last_error_at_ms
FROM provider_binding_contact
ORDER BY binding_id;
