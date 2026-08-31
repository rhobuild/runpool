-- Instance metadata and operator-facing persisted observations.

-- name: GetMetaValue :one
SELECT v FROM meta WHERE k = @key;

-- name: UpsertMetaValue :exec
INSERT INTO meta (k, v) VALUES (@key, @value)
ON CONFLICT (k) DO UPDATE SET v = excluded.v;

-- name: UpsertPressure :exec
INSERT INTO pressure (id, level, free_bytes, free_inodes, managed_bytes, measured_at)
VALUES (1, @level, @free_bytes, @free_inodes, @managed_bytes, unixepoch())
ON CONFLICT (id) DO UPDATE SET
  level = excluded.level,
  free_bytes = excluded.free_bytes,
  free_inodes = excluded.free_inodes,
  managed_bytes = excluded.managed_bytes,
  measured_at = excluded.measured_at;

-- name: GetPressure :one
SELECT id, level, free_bytes, free_inodes, managed_bytes, measured_at
FROM pressure WHERE id = 1;

-- name: InsertAuditEntry :exec
INSERT INTO audit_log (actor, action, subject, detail)
VALUES (@actor, @action, @subject, @detail);

-- name: ListAuditTail :many
SELECT id, at, actor, action, subject, detail
FROM audit_log
ORDER BY at DESC, id DESC
LIMIT @row_limit;
