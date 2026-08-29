-- Delivery fingerprints written by schema version 1 used the delimiter
-- encoding. Versioning the digest keeps those rows comparable while new
-- deliveries use the length-prefixed canonical encoding.
ALTER TABLE broker_deliveries
	ADD COLUMN payload_fingerprint_version INTEGER NOT NULL DEFAULT 1
		CHECK (payload_fingerprint_version IN (1, 2));
