# Delivery fingerprints name their encoding

**Status:** implemented
**Date:** 2026-08-29

Each broker delivery persists both its SHA-256 digest and the semantic name of
the canonical payload encoding that produced it. New deliveries use
`length-prefixed-sha256`. A redelivery is recomputed with the format already
stored on its row; the row is never rewritten merely because another encoding
exists.

The delimiter-separated encoder is the format shipped with schema version 1.
The current encoder writes every string and collection with a fixed-width
length, encodes signed correlation numbers as fixed-width values, sorts encoded
workloads and labels, and begins with a format-specific domain separator.

## What forced it

The digest is a contract-drift detector, so its preimage must distinguish
structures rather than only their printed form. The original encoder joined
fields with `|`, labels with `,`, and workloads with a newline. Consequently,
one label `a,b` and two labels `a` and `b` produced identical bytes before
hashing. Delimiters inside opaque provider keys created the same ambiguity.

Changing the algorithm without storing its format would make an exact
redelivery of a row written by an earlier release look like upstream drift.
The core store cannot safely recompute historical rows: some fields covered by
the digest live only in adapter-owned metadata, and a migration does not hold
the provider payload that supplied their ordering and absence rules.

An ordinal such as `1`, `2` or `V10` does not describe the representation and
turns every reader into a lookup table for chronology. The durable vocabulary
therefore names the actual encoding. The current choice is selected once for
new writes, while a registry maps every readable format name to its immutable
encoder.

## What was rejected

**Changing the delivery key.** That would record the same broker message as a
new delivery. Attempt uniqueness may prevent duplicate execution, but it would
also discard the drift check precisely where compatibility is needed.

**Rewriting every historical digest.** The original payload is not retained as
a canonical blob, so a rewrite would reconstruct provider input from a lossy
projection and call the result authoritative.

**Canonical JSON.** JSON would add another normalization contract for numbers,
escaping, object keys and absent values. The binary encoding is smaller and its
injectivity follows directly from its length prefixes.

## What follows from it

Schema version 2 adds `payload_fingerprint_format` with
`delimiter-separated-sha256` as the default, preserving every schema-version-1
row. New rows persist `length-prefixed-sha256`. Both encodings have golden
tests, and the delimiter implementation remains compatibility code rather than
the first member of an ordinal naming sequence.

Any future encoding change adds a name that describes its representation,
keeps every published encoder readable, and changes the single current-format
selector. It never edits a published migration or silently changes the bytes
identified by an existing name.
