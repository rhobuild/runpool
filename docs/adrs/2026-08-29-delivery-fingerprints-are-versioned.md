# Delivery fingerprints carry their encoding version

**Status:** implemented
**Date:** 2026-08-29

Each broker delivery persists both its SHA-256 digest and the version of the
canonical payload encoding that produced it. New deliveries use the current
length-prefixed encoder. A redelivery is recomputed with the version already
stored on its row; the row is never rewritten merely because a newer encoding
exists.

The delimiter encoder is the format shipped with schema version 1. The current
encoder writes every string and collection with a fixed-width length, encodes
signed correlation numbers as fixed-width values, sorts encoded workloads and
labels, and begins with a version-specific domain separator.

## What forced it

The digest is a contract-drift detector, so its preimage must distinguish
structures rather than only their printed form. The original encoder joined
fields with `|`, labels with `,`, and workloads with a newline. Consequently
one label `a,b` and two labels `a` and `b` produced identical bytes before
hashing. Delimiters inside opaque provider keys created the same class of
ambiguity.

Changing the algorithm without storing its version would make an exact
redelivery of a row written by an earlier release look like upstream drift.
The core store cannot safely recompute and rewrite historical rows: some fields
covered by the digest live only in adapter-owned metadata, and a migration does
not have the provider payload that supplied their ordering and absence rules.

## What was rejected

**Bumping the delivery key.** That would record the same broker message as a
new delivery. Attempt uniqueness may prevent duplicate execution, but it would
also discard the drift check precisely where compatibility is needed.

**Rewriting every historical digest.** The original payload is not retained as
a canonical blob, so a rewrite would reconstruct provider input from a lossy
projection and call the result authoritative.

**Canonical JSON.** JSON would add another normalization contract for numbers,
escaping, object keys and absent values. The binary encoding is smaller and its
injectivity follows directly from its length prefixes.

## What follows from it

Schema version 2 adds `payload_fingerprint_version` with default `1`, preserving
every schema-version-1 row. New rows persist the registry's current value. Both
encodings have golden tests, and the delimiter implementation remains private
compatibility code rather than a growing set of public version symbols.

Any future encoding change adds a new member to the persisted vocabulary,
keeps every previously published encoder readable, and advances the current
version. It never edits a published migration or silently changes an existing
version's bytes.
