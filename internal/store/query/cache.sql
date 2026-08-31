-- Cache projects and their exclusive warm lanes.

-- name: GetCacheProjectID :one
SELECT id FROM cache_projects WHERE source_project_key = @source_project_key;

-- name: InsertCacheProject :exec
INSERT INTO cache_projects (id, source_project_key)
VALUES (@id, @source_project_key);

-- name: FindFreeCacheLaneID :one
SELECT id FROM cache_lanes
WHERE project_id = @project_id
  AND generation = @generation
  AND leased_by IS NULL
ORDER BY last_used DESC
LIMIT 1;

-- name: ClaimCacheLane :execrows
UPDATE cache_lanes
SET leased_by = @lease_id, last_used = unixepoch()
WHERE id = @id AND leased_by IS NULL;

-- name: CountCacheLanes :one
SELECT count(*) FROM cache_lanes
WHERE project_id = @project_id AND generation = @generation;

-- name: InsertCacheLane :exec
INSERT INTO cache_lanes (id, project_id, generation, leased_by)
VALUES (@id, @project_id, @generation, @lease_id);

-- name: BackdateCacheLane :execrows
UPDATE cache_lanes
SET last_used = @last_used
WHERE id = @id;

-- name: ReleaseCacheLane :exec
UPDATE cache_lanes SET leased_by = NULL WHERE leased_by = @lease_id;

-- name: DeleteFreeCacheLane :execrows
DELETE FROM cache_lanes WHERE id = @id AND leased_by IS NULL;

-- name: ListCacheLanes :many
SELECT l.id, p.source_project_key, l.generation,
       coalesce(l.leased_by, '') AS leased_by, l.last_used
FROM cache_lanes l
JOIN cache_projects p ON p.id = l.project_id
ORDER BY p.source_project_key, l.generation, l.id;
