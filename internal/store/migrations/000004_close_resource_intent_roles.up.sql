-- Resource intents describe lease-scoped capsule resources. Close role at the
-- persistence boundary just as kind and state already are: instance resources
-- such as uplinks, cache lanes and probes are discovered from engine ownership
-- labels and must never appear in this saga.
--
-- SQLite cannot add a CHECK to an existing column, so rebuild this leaf table
-- and recreate its indexes. The migration fails closed if a deployed database
-- contains a role this build cannot safely reconcile.
CREATE TABLE resource_intents_v4 (
	id          INTEGER PRIMARY KEY,
	lease_id    TEXT NOT NULL REFERENCES capsule_leases (id),
	kind        TEXT NOT NULL CHECK (kind IN ('container', 'network', 'volume')),
	role        TEXT NOT NULL CHECK (role IN ('capsule', 'gateway', 'capsule-net', 'dind-data')),
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

INSERT INTO resource_intents_v4 (
	id, lease_id, kind, role, name, docker_id, state, retries,
	last_error, not_before, created_at, updated_at
)
SELECT id, lease_id, kind, role, name, docker_id, state, retries,
	last_error, not_before, created_at, updated_at
FROM resource_intents;

DROP TABLE resource_intents;
ALTER TABLE resource_intents_v4 RENAME TO resource_intents;

CREATE INDEX resource_intents_by_lease ON resource_intents (lease_id);

CREATE INDEX resource_intents_pending ON resource_intents (state, not_before)
	WHERE state IN ('cleanup_pending', 'deleting');
