-- Schema version 1 wrote delimiter-separated delivery fingerprints and used
-- ordinal prefixes for two durable composite keys. Name each representation
-- by the fields or encoding it contains so future formats remain intelligible
-- without positional identifiers.
ALTER TABLE broker_deliveries
	ADD COLUMN payload_fingerprint_format TEXT NOT NULL
		DEFAULT 'delimiter-separated-sha256'
		CHECK (payload_fingerprint_format IN (
			'delimiter-separated-sha256', 'length-prefixed-sha256'));

-- Both key rewrites preserve the fields and row ids. They prevent an upgrade
-- from treating an existing delivery as new or an existing binding as a
-- different configured source.
UPDATE provider_bindings
SET source_binding_key = 'target-runner-group-scale-set|' || substr(source_binding_key, 4)
WHERE source_binding_key LIKE 'v2|%';

UPDATE broker_deliveries
SET source_delivery_key = 'queue-delivery|' || substr(source_delivery_key, 4)
WHERE source_delivery_key LIKE 'v2|%';
