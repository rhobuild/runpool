# Security review scope

[Release readiness](../release-readiness.md) holds publication until findings
affecting the release boundary are resolved or explicitly accepted. This
document says where that boundary runs, what already stands as evidence, and
what a reviewer is being asked to answer.

It exists so a review can start from the interesting questions instead of
rediscovering the architecture.

## What Runpool claims, and does not

Runpool is a resource and hygiene boundary for CI that is already trusted. It
is **not** a sandbox for hostile code, and the deployment manifest says so
where an operator will read it.

Two facts set the ceiling on any isolation claim:

- The controller holds the host's Docker socket. That confers host-root
  authority, and mounting the socket read-only restricts replacing the file,
  not the API behind it.
- Every job capsule runs a privileged Docker daemon so a workload can build
  and run containers.

A review that concludes "a malicious workflow could reach the host" is
restating the design. The useful questions are narrower, and they are below.

## The boundary

Four surfaces decide whether Runpool keeps the promises it does make.

**Admission.** Whether work that does not fit is refused before it starts
rather than degrading everything else. Preflight compares the worst admitted
workload set plus the host reserve against real kernel figures, and the
comparison is strict.

**The capsule envelope.** Whether a job — runner, inner daemon, child
containers and egress gateway — stays inside one aggregate cgroup budget, and
whether nothing survives to the next job: workspace, daemon data root, images,
build cache, runner bootstrap state.

**Egress.** Whether the default profile actually confines a job to the
destinations and ports the policy allows, through kernel-enforced no-route
isolation plus a DNS and HTTP relay that applies the policy rather than
advertising it.

**Ownership.** Whether cleanup only ever touches resources carrying this
instance's labels, re-inspects ownership before each removal, and never issues
a daemon-wide prune. On a shared daemon this is what stands between Runpool and
its neighbours.

## Evidence that already exists

A reviewer should treat these as claims to test, not as answers:

- [Threat model](threat-model.md) — the properties Runpool asserts and how each
  is proved
- [Architecture](../architecture.md) — package boundaries, identity, recovery
  and capacity semantics
- The live contract suites under `test/contract/` — Docker, capsule, egress,
  cache, SQLite and lifecycle properties, run without skips on the reference
  platform during qualification
- The controller end-to-end suite under `test/e2e/controller/` — a real
  assignment through a real provider, including the sentinel assertions that
  unrelated resources survive cleanup
- Provenance and SBOM attestations produced at publication for both images and
  the standalone artifacts

## Questions a review should be able to answer

These are the ones the suites cannot settle by themselves, because each turns
on judgement rather than on a property a test can assert.

1. Does the admission arithmetic hold for every configuration the validator
   accepts, or only for the shapes the tests exercise?
2. Can a workload influence what the controller admits next — through the
   provider protocol, through resource pressure, or through what it leaves
   behind?
3. Does the egress relay's policy decision survive connection reuse, protocol
   downgrade, and a client that does not behave?
4. Is ownership verification defeatable by a workload that creates objects
   carrying labels it should not be able to set?
5. Does recovery after a controller restart ever adopt or release a lease that
   belongs to different work?
6. Do the credentials a deployment holds — provider tokens, the Docker socket,
   the registry session during a build — have a shorter reach than the blast
   radius they would have if leaked?

## Closing a finding

A finding is closed one of two ways, and both are recorded in the
[findings register](review-findings.md):

- **Resolved** — the behaviour changed, with the commit that changed it and the
  test that would catch it returning.
- **Explicitly accepted** — the behaviour stands, with the reasoning, who
  accepted it, and what would make it unacceptable later.

Silence is not a third option. An unaddressed finding keeps the release
authorization gate closed.
