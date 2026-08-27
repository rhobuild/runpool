# Product contract

What Runpool promises, what it refuses to promise, and the words used
to tell those apart. Nothing here is aspirational: a claim appears only
once something enforces it.

## The vocabulary

| Word | Means |
| --- | --- |
| **implemented** | The code exists and the hermetic suite covers it |
| **tested live** | It also passed its contract suite against a real Linux Docker host |
| **release-qualified** | It passed the complete release workflow on the reference platform |
| **supported** | Released, qualified where required, and inside the support matrix |

Release qualification is reproducible first-party engineering evidence, not a
third-party certification. A version reaches each of these states by passing
the gates in [release readiness](release-readiness.md), and reaches none of
them by assertion.

## What Runpool promises

- **Every accepted workload gets a fresh runner, a fresh Docker daemon
  whose data root does not survive the job, and a fresh workspace.**
  Nothing carries over except an explicitly mounted cache lane.
- **A job cannot exceed its tier.** Runner, daemon, inner containers
  and the job's egress gateway are one budget, enforced by one cgroup
  hierarchy.
- **Configured parallelism is enforced at both edges.** The controller never
  advertises or locally admits aggregate work beyond the tier limits and the
  optional instance-wide limit; unreleased and recovered leases continue to
  count.
- **Runpool deletes only what it owns.** Every object carries ownership
  labels; a name match with foreign labels is refused, and no daemon
  wide prune is ever issued.
- **Accepted work is not lost.** It is durable before it is
  acknowledged, and where evidence cannot decide what happened, the
  attempt is held for a person instead of being guessed at.
- **A restart adopts running work; it never re-runs it.** A capsule that
  outlives the controller is adopted by the next start and observed to
  its real outcome. Work is requeued only when the evidence proves the
  runner never started; from the start authorization onward, at-most-once
  governs and an undecidable outcome is held for a person.
- **The host cannot be filled silently.** Admission closes before the
  disk does.
- **Under the restricted profile, a capsule has no route out.** Its
  only egress is a policy-enforcing relay.

## What Runpool refuses to promise

- **It is not a sandbox for hostile code.** The controller holds the
  Docker socket; capsules run a privileged inner daemon. It is a
  resource, hygiene and network-policy boundary for CI you already
  trust.
- **It does not support fork pull requests from public repositories**,
  per GitHub's own guidance for self-hosted runners.
- **It does not turn a shared daemon into a security boundary.**
  `shared-daemon` is an explicit coexistence contract for private, trusted CI:
  admission leaves configured host capacity unused, restricted egress is
  mandatory, and destructive operations remain instance-scoped. A controller,
  daemon or kernel compromise can still affect every service on that host.
- **It does not protect platform-wide cleanup from the platform itself.**
  Operators must not run Docker volume prune or a system prune that includes
  volumes on a shared daemon. Runpool owns collection of its cache lanes.
- **It does not provide transparent L3 egress.** Under the restricted
  profile, direct connections fail closed. Proxy-aware clients may use
  HTTP or CONNECT to allowed addresses on ports 80 and 443; CONNECT is
  an opaque tunnel. `git+ssh`, other ports, non-DNS UDP, and clients
  that ignore proxy settings do not leave. `unsafe-open-egress` is the
  explicit opt-out, and its name is the warning.
- **It does not support IPv6 for capsules.** The sandbox denies it, and
  the configuration rejects a value claiming otherwise.
- **It does not promise a compatibility window for pre-release
  schemas.** A database written by an unknown build is refused with
  instructions, not repaired.

## Compatibility surfaces, and what a version change means

Runpool uses SemVer, because these are the surfaces a consumer depends
on and SemVer is what describes them:

| Surface | Where it is defined |
| --- | --- |
| CLI commands, flags, exit codes | [CLI reference](reference/cli.md) |
| Configuration schema | [Configuration reference](reference/configuration.md); the validator is the authority |
| `status --json` document | [Status API reference](reference/status-api.md) — versioned separately as `v1` |
| On-disk state | Migrations, forward-only after a release |
| Capsule control protocol | `internal/capsule/protocol` — the version a capsule declares and the controller speaks; a capsule declaring a version this build does not speak is refused, never guessed at |
| Operational procedures | [Runbook](runbook.md) |

The configuration and status API versions move independently of the product
version. Breaking either V1 contract requires an explicit version change.

## The support matrix

See the [support matrix](reference/support-matrix.md) for what runs on
what, and [SUPPORT.md](../SUPPORT.md) for where to ask. The release-reference
platforms are exact configurations, one per platform qualified, frozen in
`build/platform.lock.json` and checked by the contract suite rather than
assumed; they do not replace the runtime compatibility range.
