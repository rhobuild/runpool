-- Code generated from internal/store/migrations (schema version 1). DO NOT EDIT.
-- Regenerate with: go run ./internal/store/schema/gen

CREATE TABLE assignment_attempts (
	id                   TEXT PRIMARY KEY,
	delivery_id          INTEGER NOT NULL,
	binding_id           INTEGER NOT NULL,
	source_workload_key  TEXT NOT NULL CHECK (length(source_workload_key) > 0),
	tenant_key           TEXT NOT NULL DEFAULT '',
	project_key          TEXT NOT NULL DEFAULT '',
	state                TEXT NOT NULL DEFAULT 'ready'
		CHECK (state IN ('ready', 'leased', 'preparing', 'prepared',
		                 'starting', 'running', 'manual_review',
		                 'superseded', 'settled', 'canceled')),
	execution_evidence   TEXT NOT NULL DEFAULT 'not_started'
		CHECK (execution_evidence IN
			('not_started', 'runtime_prepared', 'execution_start_authorized',
			 'running_observed', 'exit_observed')),
	resolution           TEXT NOT NULL DEFAULT '',
	review_reason        TEXT NOT NULL DEFAULT '',
	reviewed_at          INTEGER,
	reviewed_by          TEXT NOT NULL DEFAULT '',
	received_at          INTEGER NOT NULL DEFAULT (unixepoch()),
	settled_at           INTEGER,
	FOREIGN KEY (delivery_id, binding_id)
		REFERENCES broker_deliveries (id, binding_id),
	UNIQUE (delivery_id, source_workload_key)
);

CREATE TABLE attempt_events (
	id               INTEGER PRIMARY KEY,
	attempt_id       TEXT NOT NULL REFERENCES assignment_attempts (id),
	idempotency_key  TEXT NOT NULL,
	kind             TEXT NOT NULL CHECK (kind IN (
		'attempt_created', 'lease_attached', 'runtime_prepared',
		'execution_start_authorized', 'running_observed', 'exit_observed',
		'runtime_observation_failed', 'cleanup_started', 'cleanup_completed',
		'manual_review_requested', 'operator_resolved', 'attempt_settled',
		'attempt_superseded', 'remote_canceled')),
	detail_json      TEXT NOT NULL DEFAULT '{}',
	created_at       INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE (attempt_id, idempotency_key)
);

CREATE TABLE audit_log (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	at      INTEGER NOT NULL DEFAULT (unixepoch()),
	actor   TEXT    NOT NULL,
	action  TEXT    NOT NULL,
	subject TEXT    NOT NULL,
	detail  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE broker_deliveries (
	id                   INTEGER PRIMARY KEY,
	binding_id           INTEGER NOT NULL,
	source_delivery_key  TEXT NOT NULL CHECK (length(source_delivery_key) > 0),
	payload_sha256       BLOB NOT NULL CHECK (length(payload_sha256) = 32),
	ack_state            TEXT NOT NULL DEFAULT 'pending'
		CHECK (ack_state IN ('pending', 'requested', 'confirmed', 'uncertain')),
	received_at          INTEGER NOT NULL DEFAULT (unixepoch()),
	ack_updated_at       INTEGER,
	acknowledged_at      INTEGER,
	FOREIGN KEY (binding_id) REFERENCES provider_bindings (id),
	UNIQUE (binding_id, source_delivery_key),
	-- The composite key assignment_attempts points at, so an attempt can
	-- never name a delivery belonging to a different binding.
	UNIQUE (id, binding_id)
);

CREATE TABLE cache_lanes (
	id          TEXT PRIMARY KEY,
	project_id  TEXT NOT NULL REFERENCES cache_projects (id),
	generation  TEXT NOT NULL,
	leased_by   TEXT,
	last_used   INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE cache_projects (
	id                  TEXT PRIMARY KEY,
	source_project_key  TEXT NOT NULL UNIQUE,
	created_at          INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE capsule_leases (
	id            TEXT PRIMARY KEY,
	binding_id    INTEGER NOT NULL REFERENCES provider_bindings (id),
	attempt_id    TEXT NOT NULL REFERENCES assignment_attempts (id),
	tier_id       TEXT NOT NULL,
	state         TEXT NOT NULL CHECK (state IN (
		'reserved', 'provisioning', 'runtime_registered', 'workload_running',
		'draining', 'cleaning', 'released', 'failed', 'quarantined')),
	-- The name the runtime registered under. Lifecycle events correlate
	-- by workload key; this is the cross-check that says a runner is
	-- executing the workload it was provisioned for.
	runtime_name  TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL DEFAULT (unixepoch()),
	updated_at    INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE github_actions_attempt_metadata (
	attempt_id         TEXT PRIMARY KEY REFERENCES assignment_attempts (id),
	job_id             TEXT NOT NULL CHECK (length(job_id) > 0),
	runner_request_id  INTEGER NOT NULL DEFAULT 0,
	workflow_run_id    INTEGER NOT NULL DEFAULT 0,
	-- The ephemeral runner GitHub assigned when the capsule registered.
	-- Zero until registration succeeds; it is what deregistration needs
	-- when a capsule fails before GitHub expires the runner itself.
	runner_id          INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE github_actions_binding_metadata (
	binding_id      INTEGER PRIMARY KEY REFERENCES provider_bindings (id),
	scope           TEXT NOT NULL CHECK (scope IN ('repository', 'organization', 'enterprise')),
	canonical_url   TEXT NOT NULL,
	runner_group    TEXT NOT NULL DEFAULT '',
	scale_set_name  TEXT NOT NULL,
	scale_set_id    INTEGER CHECK (scale_set_id IS NULL OR scale_set_id > 0),
	UNIQUE (canonical_url, runner_group, scale_set_name)
);

CREATE TABLE meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);

CREATE TABLE pressure (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	level         TEXT    NOT NULL,
	free_bytes    INTEGER NOT NULL,
	free_inodes   INTEGER NOT NULL,
	managed_bytes INTEGER NOT NULL,
	measured_at   INTEGER NOT NULL
);

CREATE TABLE provider_binding_contact (
	binding_id          INTEGER PRIMARY KEY REFERENCES provider_bindings (id),
	last_contact_at_ms  INTEGER NOT NULL DEFAULT 0,
	last_error          TEXT NOT NULL DEFAULT '',
	last_error_at_ms    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE provider_bindings (
	id                  INTEGER PRIMARY KEY,
	target_id           TEXT NOT NULL,
	provider_kind       TEXT NOT NULL CHECK (length(provider_kind) > 0),
	source_binding_key  TEXT NOT NULL CHECK (length(source_binding_key) > 0),
	created_at          INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE (provider_kind, source_binding_key)
);

CREATE TABLE resource_intents (
	id          INTEGER PRIMARY KEY,
	lease_id    TEXT NOT NULL REFERENCES capsule_leases (id),
	kind        TEXT NOT NULL CHECK (kind IN ('container', 'network', 'volume')),
	role        TEXT NOT NULL,
	name        TEXT NOT NULL CHECK (length(name) > 0),
	docker_id   TEXT NOT NULL DEFAULT '',
	state       TEXT NOT NULL DEFAULT 'planned'
		CHECK (state IN ('planned', 'creating', 'present', 'cleanup_pending', 'deleting')),
	retries     INTEGER NOT NULL DEFAULT 0,
	last_error  TEXT NOT NULL DEFAULT '',
	not_before  INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
	updated_at  INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE (lease_id, kind, name)
);

CREATE INDEX attempts_manual_review
ON assignment_attempts (received_at, id)
WHERE state = 'manual_review';

CREATE INDEX attempts_ready
ON assignment_attempts (binding_id, received_at, id)
WHERE state = 'ready';

CREATE INDEX cache_lanes_pool ON cache_lanes (project_id, generation, leased_by);

CREATE INDEX capsule_leases_by_attempt ON capsule_leases (attempt_id);

CREATE INDEX capsule_leases_by_runtime_name ON capsule_leases (runtime_name);

CREATE INDEX capsule_leases_by_state ON capsule_leases (state);

CREATE INDEX capsule_leases_released_by_age ON capsule_leases (state, updated_at)
	WHERE state = 'released';

CREATE UNIQUE INDEX one_live_lease_per_attempt
ON capsule_leases (attempt_id)
WHERE state <> 'released';

CREATE UNIQUE INDEX one_open_attempt_per_workload
ON assignment_attempts (binding_id, source_workload_key)
WHERE state IN ('ready', 'leased', 'preparing', 'prepared',
                'starting', 'running', 'manual_review');

CREATE INDEX resource_intents_by_lease ON resource_intents (lease_id);

CREATE INDEX resource_intents_pending ON resource_intents (state, not_before)
	WHERE state IN ('cleanup_pending', 'deleting');
