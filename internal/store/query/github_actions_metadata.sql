-- GitHub Actions metadata, 1:1 with core rows and owned by the adapter.
-- Diagnostic identity only: nothing here participates in the state
-- machine or substitutes for the core natural keys.

-- name: UpsertGitHubBindingMetadata :exec
INSERT INTO github_actions_binding_metadata
	(binding_id, scope, canonical_url, runner_group, scale_set_name, scale_set_id)
VALUES
	(@binding_id, @scope, @canonical_url, @runner_group, @scale_set_name, @scale_set_id)
ON CONFLICT (binding_id) DO UPDATE SET
	scope = excluded.scope,
	canonical_url = excluded.canonical_url,
	runner_group = excluded.runner_group,
	scale_set_name = excluded.scale_set_name,
	scale_set_id = excluded.scale_set_id;

-- name: ListGitHubBindingMetadata :many
SELECT binding_id, scope, canonical_url, runner_group, scale_set_name, scale_set_id
FROM github_actions_binding_metadata
ORDER BY canonical_url, runner_group, scale_set_name;

-- name: GetGitHubBindingMetadata :one
SELECT binding_id, scope, canonical_url, runner_group, scale_set_name, scale_set_id
FROM github_actions_binding_metadata
WHERE binding_id = @binding_id;

-- name: UpsertGitHubAttemptMetadata :exec
-- runner_id is deliberately absent from the conflict update: it is
-- learned later, at registration, and a redelivery re-running this
-- statement must not erase the id deregistration depends on.
INSERT INTO github_actions_attempt_metadata
	(attempt_id, job_id, runner_request_id, workflow_run_id)
VALUES
	(@attempt_id, @job_id, @runner_request_id, @workflow_run_id)
ON CONFLICT (attempt_id) DO UPDATE SET
	job_id = excluded.job_id,
	runner_request_id = excluded.runner_request_id,
	workflow_run_id = excluded.workflow_run_id;

-- name: SetGitHubAttemptRunner :execrows
-- Records the ephemeral runner GitHub assigned when the capsule
-- registered. It is what deregistration addresses when a capsule fails
-- before GitHub expires the runner on its own.
UPDATE github_actions_attempt_metadata
SET runner_id = @runner_id
WHERE attempt_id = @attempt_id;

-- name: GetGitHubAttemptMetadata :one
SELECT attempt_id, job_id, runner_request_id, workflow_run_id, runner_id
FROM github_actions_attempt_metadata
WHERE attempt_id = @attempt_id;
