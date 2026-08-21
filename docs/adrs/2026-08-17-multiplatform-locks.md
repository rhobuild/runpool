# A lock records the platforms that were qualified, not the only one that works

**Status:** accepted; the lock shape and the reader are built, the
per-platform publishing is not
**Date:** 2026-08-17

`build/platform.lock.json` carries a single `policy.arch`, so one lock
describes one platform. `build/images.lock.json` carries a single
top-level `platform`. The maintainer guide states the consequence
plainly: an arm64 host fails the gate "regardless of how well it runs
the suites".

That sentence is the problem. A gate that fails a host no matter how
well it runs the suites is not measuring the host — it is measuring
whether the host is the one platform the file can express. The
qualification is evidence that the release ran correctly somewhere; the
file's shape turns it into a claim that it runs correctly nowhere else.

## What is actually platform-bound, and what is not

The pinned base images are not. `images.lock.json` records
`sha256:0cfdcc70…` for the runner and the corresponding digest for dind,
and both are **image index** digests, not per-architecture manifests. A
build targeting `linux/arm64` resolves the arm64 child of the same
pinned digest. The Dockerfile needs no change, and the digests need no
per-platform variant: they are already platform-agnostic.

The upstream ceiling is the runner image, which publishes `linux/amd64`
and `linux/arm64` and nothing else. dind additionally publishes armv6
and armv7, so the intersection is two platforms.

The controller binary is not platform-bound in an interesting way
either. It embeds one capsule reference through `-X main.capsuleImage`.
If that reference is the capsule's index digest, one string is correct
for every architecture: the daemon resolves the matching child at pull
time, and the digest is still exact and immutable.

What is genuinely platform-bound is the observed evidence: kernel,
cgroup driver, engine build, firewall backend, storage driver. Those are
facts about one host, and there is one set of them per platform
qualified.

## Decision

**`platform.lock.json` records a list of qualified platforms.** Each
entry carries its own policy and its own observed facts. `verify
-observed` selects the entry matching the platform it is running on and
compares against that entry; a platform with no entry fails as "not
qualified on this platform", naming the ones that are, rather than as an
architecture mismatch.

**`images.lock.json` declares the platforms the release builds for**,
while the digests stay as they are, because they already describe every
platform.

**The release publishes an index per image** covering the declared
platforms, and the standalone binaries are built per platform. All of
them carry the same capsule index digest.

**A published image is not a qualified platform**, and the two are
stated separately. The support matrix names what the gates observed,
with its evidence, and names separately what is built and published
without that evidence. Neither sentence promises the other.

## Consequences

- Qualifying a second platform becomes an added entry rather than a
  schema change, which matters because the lock is a versioned format
  and changing its shape later is a migration of the file that is the
  proof of the gate.
- Two qualification hosts can be recorded at once, so a release is not
  forced to serialize them.
- An operator on arm64 can pull the published images. Whether they were
  observed by the gates is a question the support matrix answers
  honestly instead of a question the lock's shape answers by refusing.
- The false claim disappears from the maintainer guide: architecture is
  no longer checked literally against a single value, it selects which
  recorded evidence applies.
- Nothing is asserted about a platform that was never run. An
  unqualified platform is unverified, which is neither a promise nor a
  denial.

## What is built

The first, second and fourth decisions above. `platform.lock.json`
records a list of entries, each with its own policy and its own facts;
the reader selects by the platform being measured and reports a platform
with no entry as one nobody has qualified rather than as an architecture
mismatch. `images.lock.json` declares the platforms a release builds for.
The support matrix states the two lists separately.

Two things were loosened beyond what this record asked for, on the same
argument it makes. The distribution was hard-coded next to the
architecture, so a host on another one could not be recorded either; the
reader now requires the selection to be *stated* rather than to be a
particular value. And the operating system is derived from what the
pinned images publish instead of being written as a rule, because that
is where it actually comes from.

## What is not

The third decision: the release publishing an index per image. The
standalone binaries are already cross-built per platform, and the capsule
image builds for `linux/arm64` — verified, with the Go stage compiling
rather than reusing a cached layer. What is not done is the publishing
itself, which needs `buildx` with a push and a different way of reading
back the digest, and whose only real verification is a release run.

Until it lands, a release publishes one platform's image while the lock
declares two. The support matrix's *built* list is therefore a statement
about what the build can produce, not about what the last release
pushed.
