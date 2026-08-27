# Release readiness

A version is **release-qualified** only when every gate below has passed for
its exact commit, image digests and host platform. This document contains the
objective publication gates; it is not a development log.

## Release target

The first release is `v1.0.0`. Docker Engine 29.7.2, the latest official
stable Debian 13 package at the 2026-08-16 policy review, is selected for the
first release qualification. The exact platform reference in
[`build/platform.lock.json`](../build/platform.lock.json) is `frozen`: the
production-class host's kernel, API, runtime, cgroup, storage, firewall,
Buildx and Compose facts were captured on 2026-08-26 and reviewed before
any candidate tag exists, which is the point of freezing them there.
Updating Docker after this freeze requires a new reviewed lock and a
complete no-skip qualification run.

That exact host is an evidence reference, not an operator-side version pin.
Runtime admission requires Docker Engine 28.0 or newer and the capabilities it
uses. A newer daemon is not rejected for being newer, but it does not become a
tested or supported platform until it passes the relevant live matrix.

## Required gates

| Gate | Current state | Completion evidence |
| --- | --- | --- |
| Hermetic CI | Implemented | `ci.yml` passes formatting, vet, staticcheck, race, coverage, sqlc parity, builds, link checks, vulnerability scan, and image build |
| Live Docker contracts | Implemented; qualification not executed | Docker, capsule, egress, cache, SQLite, and lifecycle suites pass without skips on the reference host |
| Upstream provider contracts | Implemented; protected fixtures required | Live GitHub Actions contracts pass without skips using short-lived GitHub App installation tokens |
| Controller end-to-end workload | Implemented; qualification not executed | The exact controller runs in `shared-daemon`, receives real fixture assignments, launches the exact capsule candidate under restricted egress, completes checkout, dependency download, Docker build and registry push, proves cache reuse and generation isolation, survives controller restart, removes owned resources, and preserves unrelated container, network and volume sentinels by exact id |
| Host topology contracts | Implemented; qualification not executed | Shared mode enforces positive reserves, restricted egress, organization runner-group isolation, ownership-verified cleanup and idle-uplink recovery; the shared controller E2E and common Docker contracts pass on the reference host |
| Scheduling and swap envelope | Implemented; live qualification not executed | Global and tier parallelism constrain provider announcements and local admission through restart and quarantine; preflight proves the worst admitted CPU, memory, and swap set plus host reserve fits; configured swap is enforced in a real capsule on the reference host |
| Immutable release candidates | Implemented; qualification not executed | Controller and capsule images are built before qualification and identified by digest; the standalone binary and completions are retained by checksum; publication promotes or downloads those exact bytes without rebuilding |
| Exact platform match | **Met** — Engine 29.7.2 frozen from the reference host | The reviewed lock is frozen before the candidate; every observed fact matches and missing facts fail the gate |
| External security review | **Met** — one review against `c8ac420`, no blocking finding | Findings affecting the release boundary are [resolved or explicitly accepted](security/review-findings.md) before release approval |
| Release authorization | Outstanding | The protected `release` environment approves publication only after all prior gates pass |

The release-qualification host also needs GitHub Actions Runner 2.327.1 or
newer and access to protected upstream and end-to-end fixtures. These are
infrastructure prerequisites, not substitutes for a passing gate.

## Release mechanics

A protected SemVer tag starts the release workflow. The workflow:

1. builds and pushes immutable controller and capsule candidates for that
   commit, and builds the standalone release artifacts once;
2. qualifies that exact commit, both image digests, and the retained artifact
   checksums on the reference host;
3. requires hermetic CI, live Docker and provider contracts, and the real
   controller end-to-end workload with no skipped required contract;
4. creates `release-qualification.json` from the evidence and verifies it
   against the commit and candidate digests;
5. promotes the qualified image digests and publishes the already-built
   standalone artifacts without rebuilding them;
6. creates provenance and SBOM attestations; and
7. creates a draft GitHub release behind the protected `release` environment.

The workflow cannot publish from a branch, a different commit, an unmeasured
platform, a different image, or a post-qualification rebuild.

## Release decision

Do not publish while any required gate above is outstanding. Green hermetic CI
is necessary but does not substitute for the controller end-to-end evidence,
the external security review, or the no-skip live release-qualification run.
