-- Runpool's durable state.
--
-- This baseline defines schema version 1. Once the first release is
-- published it becomes immutable; subsequent changes use new forward-only
-- migrations. Restoring the pre-migration backup is the rollback path.
--
-- Two vocabularies live here and never mix. The core tables speak
-- binding, delivery, workload, attempt, lease and runtime. Provider
-- identifiers — GitHub's scale sets, runner ids, workflow runs — live
-- only in the github_actions_* tables, which the adapter owns and
-- nothing in the core reads.

-- meta holds instance-scoped singletons, chiefly the instance id stamped
-- on every Docker object this controller owns.
CREATE TABLE meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);

-- provider_bindings is one configured place work arrives from, in
-- neutral terms. source_binding_key is whatever the provider calls it,
-- opaque here; provider_kind says who is allowed to interpret it.
CREATE TABLE provider_bindings (
	id                  INTEGER PRIMARY KEY,
	target_id           TEXT NOT NULL,
	provider_kind       TEXT NOT NULL CHECK (length(provider_kind) > 0),
	source_binding_key  TEXT NOT NULL CHECK (length(source_binding_key) > 0),
	desired_state       TEXT NOT NULL DEFAULT 'present'
		CHECK (desired_state IN ('present', 'absent')),
	created_at          INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE (provider_kind, source_binding_key)
);

-- provider_binding_contact is what a binding's own loop last managed with
-- its provider, recorded here because that is the only place a reporting
-- command can read it: a controller that is running and reaching nothing
-- holds no leases and answers every local query, which is exactly what a
-- controller with no work to do looks like. Contact and failure are kept
-- as facts rather than as a derived state, so a report can say both when
-- the last success was and what is failing now.
-- The two moments are milliseconds, and say so in their names: a report
-- decides "is this binding failing" by comparing them, and two events one
-- loop pass apart routinely land in the same second. At second
-- resolution the comparison cannot separate them and the failure loses to
-- the success that preceded it.
CREATE TABLE provider_binding_contact (
	binding_id          INTEGER PRIMARY KEY REFERENCES provider_bindings (id),
	last_contact_at_ms  INTEGER NOT NULL DEFAULT 0,
	last_error          TEXT NOT NULL DEFAULT '',
	last_error_at_ms    INTEGER NOT NULL DEFAULT 0
);

-- broker_deliveries is one message from a provider, made durable before
-- it is acknowledged. payload_sha256 is the drift detector: the same
-- natural key arriving with different content is a broken upstream
-- assumption, not an update.
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

-- assignment_attempts is one execution attempt for one workload, and the
-- sole owner of both what was observed about it (execution_evidence) and
-- how it ended (resolution, review_reason, settled_at). The lease is
-- runtime plumbing that comes and goes; the attempt is the record.
--
-- execution_evidence is strictly monotonic and names observations only.
-- There is no value for "could not observe": an unobservable outcome is
-- an operational condition, recorded as manual_review with a reason, not
-- a rung that could displace something that was actually seen.
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

-- attempt_events is the append-only audit trail. idempotency_key makes a
-- replayed transition record once, so a retried write cannot inflate the
-- history an operator reads back.
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

-- capsule_leases is the lifecycle of the host resources one attempt
-- consumes: a physical slot is held before the row is committed, and
-- every Docker object created for it is recorded against it until
-- cleanup releases them.
--
-- It carries no provider identifiers. binding_id says which binding's
-- runtime this is — the reconciler needs it to find the client that can
-- clean up — and attempt_id is the single link between the two rows.
-- The link lives here because the lease is the later, shorter-lived row.
--
-- An attempt whose work provably never began returns to the servable
-- queue and is served again, so it holds one lease per serving over its
-- life. One of them may be live at a time, which the partial unique index
-- below enforces; the rest are the history of what this attempt cost.
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

-- resource_intents is the durable saga for external effects: the row is
-- committed before the create call and deleted only when the object is
-- proven gone. name is deterministic and is the recovery handle for the
-- window where existence is ambiguous; docker_id is set once confirmed.
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

-- cache_projects maps a project's opaque source key to a stable local
-- id. The id, not the key, names the lane volumes, so a rename upstream
-- never orphans warm data.
CREATE TABLE cache_projects (
	id                  TEXT PRIMARY KEY,
	source_project_key  TEXT NOT NULL UNIQUE,
	created_at          INTEGER NOT NULL DEFAULT (unixepoch())
);

-- cache_lanes is one exclusive warm lane. leased_by is the lease holding
-- it (NULL is free) and deliberately carries no foreign key: a lane's
-- data outlives the lease that filled it, which is the entire point.
CREATE TABLE cache_lanes (
	id          TEXT PRIMARY KEY,
	project_id  TEXT NOT NULL REFERENCES cache_projects (id),
	generation  TEXT NOT NULL,
	leased_by   TEXT,
	last_used   INTEGER NOT NULL DEFAULT (unixepoch())
);

-- pressure is the disk monitor's last verdict, durable so an emergency
-- survives the process that declared it.
CREATE TABLE pressure (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	level         TEXT    NOT NULL,
	free_bytes    INTEGER NOT NULL,
	free_inodes   INTEGER NOT NULL,
	managed_bytes INTEGER NOT NULL,
	measured_at   INTEGER NOT NULL
);

-- audit_log records maintenance actions against durable resources that
-- no attempt can carry — evicting a cache lane, forcing a cleanup.
CREATE TABLE audit_log (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	at      INTEGER NOT NULL DEFAULT (unixepoch()),
	actor   TEXT    NOT NULL,
	action  TEXT    NOT NULL,
	subject TEXT    NOT NULL,
	detail  TEXT    NOT NULL DEFAULT ''
);

-- Adapter-owned extensions. These are 1:1 with a core row and hold the
-- provider's own identifiers. Nothing in the core reads them; they exist
-- so the adapter can address GitHub, and so an operator can correlate a
-- Runpool attempt with a workflow run.
CREATE TABLE github_actions_binding_metadata (
	binding_id      INTEGER PRIMARY KEY REFERENCES provider_bindings (id),
	scope           TEXT NOT NULL CHECK (scope IN ('repository', 'organization', 'enterprise')),
	canonical_url   TEXT NOT NULL,
	runner_group    TEXT NOT NULL DEFAULT '',
	scale_set_name  TEXT NOT NULL,
	scale_set_id    INTEGER CHECK (scale_set_id IS NULL OR scale_set_id > 0),
	UNIQUE (canonical_url, runner_group, scale_set_name)
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

CREATE INDEX attempts_ready
ON assignment_attempts (binding_id, received_at, id)
WHERE state = 'ready';

CREATE INDEX attempts_manual_review
ON assignment_attempts (received_at, id)
WHERE state = 'manual_review';

-- One live attempt per workload. This partial index is the invariant
-- that keeps a job from running twice: everything outside the listed
-- states is resolved and no longer competes.
CREATE UNIQUE INDEX one_open_attempt_per_workload
ON assignment_attempts (binding_id, source_workload_key)
WHERE state IN ('ready', 'leased', 'preparing', 'prepared',
                'starting', 'running', 'manual_review');

-- One live lease per attempt. The sibling of one_open_attempt_per_workload,
-- and for the same reason: a released lease is finished history and no
-- longer competes, so it is what makes room for the next serving. Every
-- path back to `ready` is covered by this rather than by each of them
-- remembering to make room.
CREATE UNIQUE INDEX one_live_lease_per_attempt
ON capsule_leases (attempt_id)
WHERE state <> 'released';

CREATE INDEX capsule_leases_by_state ON capsule_leases (state);

-- Not partial: attribution asks which attempt a runtime ran for, and a
-- late report's lease is released by the time it arrives. An index that
-- excluded released rows would leave the one query on this column a
-- full-table scan.
CREATE INDEX capsule_leases_by_runtime_name ON capsule_leases (runtime_name);

-- Released leases by age. Two readers need exactly this: reporting takes
-- the most recent ones so a snapshot stays bounded by live work rather
-- than by every job the host ever ran, and retention takes the oldest
-- ones. Without the time key a LIMIT still materializes and sorts the
-- whole released set, which on a long-lived host is the entire history.
--
-- The key is updated_at, not created_at: both readers ask when a lease
-- finished, and for a released lease that is its last transition. A
-- lease that wedged for weeks and was resolved a minute ago is recent
-- history by the first column and ancient by the second.
CREATE INDEX capsule_leases_released_by_age ON capsule_leases (state, updated_at)
	WHERE state = 'released';

CREATE INDEX resource_intents_by_lease ON resource_intents (lease_id);

CREATE INDEX resource_intents_pending ON resource_intents (state, not_before)
	WHERE state IN ('cleanup_pending', 'deleting');

CREATE INDEX cache_lanes_pool ON cache_lanes (project_id, generation, leased_by);
