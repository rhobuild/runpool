-- Broker deliveries: at-least-once messages made durable before any
-- acknowledgement, with a fingerprint that tells redelivery apart from
-- contract drift. Ack transitions are compare-and-swap on the states
-- they may leave from; zero rows means the delivery moved and the
-- caller re-reads.

-- name: InsertDelivery :one
INSERT INTO broker_deliveries (
  binding_id, source_delivery_key, payload_sha256, payload_fingerprint_version
)
VALUES (@binding_id, @source_delivery_key, @payload_sha256, @payload_fingerprint_version)
RETURNING id, binding_id, source_delivery_key, payload_sha256, ack_state,
          received_at, ack_updated_at, acknowledged_at,
          payload_fingerprint_version;

-- name: GetDeliveryByKey :one
SELECT id, binding_id, source_delivery_key, payload_sha256, ack_state,
       received_at, ack_updated_at, acknowledged_at,
       payload_fingerprint_version
FROM broker_deliveries
WHERE binding_id = @binding_id
  AND source_delivery_key = @source_delivery_key;

-- name: MarkAckRequested :execrows
-- 'requested' is included so a delivery left in flight by a crash is
-- retried. The broker was never told in that state, so it redelivers the
-- same message forever; excluding it made the retry a no-op and the
-- binding stopped processing anything behind it in the queue.
UPDATE broker_deliveries
SET ack_state = 'requested', ack_updated_at = unixepoch()
WHERE id = @delivery_id AND ack_state IN ('pending', 'requested', 'uncertain');

-- name: MarkAckConfirmed :execrows
UPDATE broker_deliveries
SET ack_state = 'confirmed', ack_updated_at = unixepoch(), acknowledged_at = unixepoch()
WHERE id = @delivery_id AND ack_state IN ('pending', 'requested', 'uncertain');

-- name: MarkAckUncertain :execrows
UPDATE broker_deliveries
SET ack_state = 'uncertain', ack_updated_at = unixepoch()
WHERE id = @delivery_id AND ack_state IN ('pending', 'requested');
