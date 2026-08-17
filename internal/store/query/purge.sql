-- Uninstall support: the operator destroying an instance takes its
-- delivery and attempt evidence with it. Children before parents; each
-- statement is total because uninstall names the whole instance.
--
-- Order matters. Resource intents reference leases, leases reference
-- attempts and bindings, attempts reference deliveries. Purging out of
-- order is a foreign-key failure, which is the schema doing its job.

-- name: PurgeResourceIntents :exec
DELETE FROM resource_intents;

-- name: PurgeLeases :exec
DELETE FROM capsule_leases;

-- name: PurgeAttemptEvents :exec
DELETE FROM attempt_events;

-- name: PurgeGitHubAttemptMetadata :exec
DELETE FROM github_actions_attempt_metadata;

-- name: PurgeAttempts :exec
DELETE FROM assignment_attempts;

-- name: PurgeDeliveries :exec
DELETE FROM broker_deliveries;

-- name: PurgeGitHubBindingMetadata :exec
DELETE FROM github_actions_binding_metadata;

-- name: PurgeBindings :exec
DELETE FROM provider_bindings;

-- Cache lanes and their projects. Uninstall removes the lane volumes, so
-- leaving the rows behind on a retained state volume strands them: every
-- row keeps a leased_by pointing at a lease that no longer exists, and no
-- supported path can reclaim one. Reuse needs leased_by IS NULL, GC only
-- considers unleased lanes, and DeleteCacheLane refuses a leased row, so
-- a reinstall would find the project already at its lane ceiling and run
-- every job uncached, forever. Lanes go before projects: they reference
-- them.
--
-- Keep these comments ASCII. sqlc slices query text by byte offset, so a
-- multi-byte character here truncates the generated SQL by as many bytes
-- as it adds: an em dash in this comment produced "DELETE FROM cache_lan".

-- name: PurgeCacheLanes :exec
DELETE FROM cache_lanes;

-- name: PurgeCacheProjects :exec
DELETE FROM cache_projects;
