# Support matrix

What Runpool runs on, in the words of the vocabulary from
[the product contract](../product-contract.md): **implemented** means
the code exists and the hermetic suite covers it; **tested live** means
it also passed its contract suite against a real Linux Docker host;
**release-qualified** means it passed the complete release gate on the
reference platform; **supported** means released, qualified where the
property requires it, and inside this matrix. Release qualification is
Runpool's reproducible engineering evidence, not a third-party product
certification.

**Nothing here is release-qualified or supported.** There is no release. This
page exists so that when there is one, the line between what was proven
and what was assumed is already drawn. Live status is in
[release readiness](../release-readiness.md).

## Runtime compatibility

Runpool requires Docker Engine 28.0 or newer because its restricted network
profile depends on the `isolated` gateway mode introduced in
[Engine 28](https://docs.docker.com/engine/release-notes/28/). The
controller negotiates a mutually supported Engine API version; it does not
require the host daemon to equal the release-qualification patch and it does
not reject a newer major solely for being newer.

Compatibility and support are evidence-based:

| Engine | Status before V1 |
| --- | --- |
| `< 28.0` | Incompatible: the required isolated bridge mode does not exist |
| `28.x` | Compatible floor implemented; not selected for the first release qualification |
| `29.7.2` | Latest official stable Debian 13 package at the 2026-08-16 policy review; selected for the first qualification, not yet qualified |
| Other `29.x` | Expected to negotiate successfully, but must pass the complete live matrix before being listed as tested or supported |
| Future majors | Unknown until their release notes and live matrix are reviewed |

[Engine 29](https://docs.docker.com/engine/release-notes/29/) includes meaningful
changes, including a different default image store for fresh installations and
an experimental nftables backend. Engine
[API negotiation](https://docs.docker.com/reference/api/engine/) makes it a
reasonable compatibility target, not an automatic security result. The
selected 29.7.2 host must record its image store and firewall backend as part
of the no-skip qualification before the support statement expands.

## Release-qualification target

The policy selects Docker Engine
[29.7.2](https://docs.docker.com/engine/release-notes/29/#2972) from Docker's
[official stable Debian channel](https://download.docker.com/linux/debian/dists/trixie/pool/stable/amd64/).
The exact platform reference is currently **pending**: the
production-class Linux host has not yet supplied the remaining facts. Release
qualification is fail-closed until those facts are reviewed and frozen in
`build/platform.lock.json` before the candidate tag. The host under test never
defines its own expectation.

| | Reference value |
| --- | --- |
| OS | Debian 13 (trixie), amd64 — the selection in the one entry recorded; exact patch pending host capture |
| Kernel | Pending host capture |
| Docker Engine | **29.7.2** selected; exact installed package pending capture |
| Engine API, containerd, runc | Pending host capture |
| cgroups | v2 required; exact driver pending capture |
| Storage and backing filesystem | Pending host capture |
| Rootless | no |
| Firewall backend | Stable Docker backend required; exact tools pending capture |
| Buildx / Compose | Pending host capture |
| Policy reviewed | 2026-08-16 |

Once frozen, every value is exact because a release record must identify what
actually ran. That exactness is an evidence constraint, not an operator-side
version pin. A later Docker update produces a new reviewed lock and record and
does not invalidate earlier release evidence.

## Host requirements

| Requirement | Why it is a requirement |
| --- | --- |
| Linux | The isolation is cgroup v2 and kernel netfilter; nothing else provides it |
| Rootful Docker Engine | The controller holds the daemon socket and capsules run a privileged inner daemon |
| cgroup v2 with `memory` and `pids` | The tier envelope is enforced by those controllers; swap accounting is additionally required when a tier configures swap |
| Explicit `host.topology` | `shared-daemon` and `dedicated-daemon` have different operational contracts; inference would hide the chosen blast radius |
| Positive host reserve in shared mode | Colocated platform and application services need CPU, memory and free disk that CI cannot schedule; optional swap reserve is checked separately |
| `public-internet-only` in shared mode | Open capsule egress to host and private networks is incompatible with the coexistence contract |
| `systemd` or `cgroupfs` driver | Read from the daemon at startup; the parent form is generated for the reported driver and verified live |

**What is built and what is qualified are two lists, and neither promises
the other.**

*Built* is [`build/images.lock.json`](../../build/images.lock.json):
`linux/amd64` and `linux/arm64`, which is what the pinned base images
publish. The controller is a static Go binary and the capsule's Dockerfile
builds for both.

*Qualified* is [`build/platform.lock.json`](../../build/platform.lock.json),
one entry per platform whose suites were run and whose host facts were
reviewed and frozen. It records `amd64` today and nothing else.

So `linux/arm64` is **buildable and unverified**: the build produces it —
the capsule image and the controller binary both compile for it — and
nobody has run the suites there. That is a different sentence from
unsupported, and the gate says which it is: a host with no entry fails as
*not qualified on this platform*, naming the ones that are, rather than
as an architecture mismatch.

**What a release actually publishes today is one platform.** The
publishing half of that decision is not built: the release workflow
builds and pushes a single image and ships a single `linux/amd64`
binary. So *built* here describes what the build can produce, not what
the last release put in the registry. Do not read it as a promise of an
arm64 artifact.

The distribution is the same kind of statement. `debian 13 trixie` is
the reviewed selection in the entry that exists, not a constraint the
reader enforces: a host on another distribution that ran the suites can
be recorded as its own entry. What no entry can name is a platform no
release builds for.

Operating system is not on that list. A capsule runs a Linux daemon and
a Linux runner inside a container, and the isolation is cgroup v2 and
kernel netfilter; there is no non-Linux variant of the base images to
build against. That is a design limit, and it is stated as a host
requirement above rather than as a policy anyone may revise.

The self-hosted release-qualification runner must use GitHub Actions Runner 2.327.1
or newer so the Node 24 runtime required by the pinned first-party actions is
available. This is CI infrastructure only; Runpool has no Node.js build or
runtime dependency.

`runpool doctor` checks the machine-verifiable requirements and refuses to
start when they are not met. In shared mode it reports a warning that capacity
and ownership controls do not isolate a daemon compromise. Release
qualification records the host inventory and preserves unrelated container,
network and volume sentinels across controller cleanup. A host that cannot
honour the machine contract must fail at startup, not midway through a job.

## Provider support

| Provider | State |
| --- | --- |
| GitHub Actions | The single adapter: implemented, tested live |
| Anything else | Not implemented and not promised |

Within GitHub, three things are configurable and only some of them have
been run. The distinction is deliberate: a target is refused for not
working, never for being unfamiliar, so what is *qualified* is stated
separately from what is *accepted*.

| Target | State |
| --- | --- |
| `github.com`, repository or organization scope | Qualified: this is what the release gates observe |
| Enterprise Cloud with data residency (`*.ghe.com`) | Accepted, unqualified. It is GitHub's own hosted service under another hostname and reaches the same endpoints |
| GitHub Enterprise Server | Accepted, unqualified. The provider client derives its API under `/api/v3`, using the same endpoints |
| Enterprise scope (`/enterprises/<name>`) | Accepted, unqualified, and the least exercised of the three: runner registration goes to an endpoint no suite in this repository reaches |

Unqualified means no gate has observed it, which is neither a promise nor
a denial. `runpool doctor` makes a real call against whatever host and
scope a deployment configures, so an operator learns in one command
rather than at the first job.

The lifecycle and state core is provider-neutral by construction — an
architecture test fails the build if a core package imports the adapter — but
public configuration currently describes GitHub Actions. A neutral domain is
not a second adapter, and this matrix does not pretend otherwise.

## Scope and feature support

| Feature | State |
| --- | --- |
| Repository-scoped scale sets | Implemented, tested live |
| Organization-scoped scale sets | Implemented, tested live |
| Persistent cache lanes | Implemented; manager and named-volume reuse pass live contracts. **Repository-scoped only** and off by default until controller end-to-end reuse is release-qualified |
| Restricted egress (`public-internet-only`) | Implemented and tested live; direct egress denied, proxy HTTP and CONNECT limited to allowed addresses on ports 80/443 |
| Open egress (`unsafe-open-egress`) | Implemented; the name is the warning, and it is logged loudly at startup |
| IPv6 for capsules | Not implemented. The sandbox denies it and the validator refuses a configuration claiming otherwise |
| Metrics endpoint | Not implemented. `runpool status` and the structured log are the interface |
| Standby / handover for a second controller | Not implemented. A second controller gets the lock error and stops |
| Shared Docker daemon | Implemented with explicit reserves, restricted egress, ownership-verified cleanup and per-launch uplink recovery; controller E2E qualification remains required |
| Dedicated Docker daemon | Implemented and recommended when the shared compromise domain is unacceptable |
| Enterprise-scoped scale sets | Accepted, unqualified — see Provider support above |
| Operator-supplied capsule image | `tiers[].capsuleImage`, digest-qualified, built from the published capsule. A tier that names one is outside the configuration the gates observed, and `runpool status` reports what each tier runs |
| GitHub App credentials | Implemented. The provider client mints and refreshes the installation token; the App path has no live coverage, because proving it end to end needs an App installed on the protected fixture |
| GPU and device passthrough | Not implemented. The tier envelope is cpu, memory, swap and pids; nothing reaches a device, and a GPU would have to be visible to the capsule and to the daemon inside it |
| Architectures other than amd64 | Not qualified. Nothing in the design restricts one — see Host requirements |

## What is explicitly out of scope

- **Public fork pull requests.** Per GitHub's own guidance for
  self-hosted runners. Runpool is a resource, hygiene and network-policy
  boundary for CI you already trust — not a sandbox for hostile code.
- **Treating shared mode as hostile-code containment.** A privileged capsule,
  controller or daemon compromise can affect colocated services.
- **Platform-wide volume prune in shared mode.** It bypasses Runpool's cache
  ownership and retention policy.
- **Transparent L3 egress** under the restricted profile: direct TCP,
  `git+ssh`, other ports, non-DNS UDP, and proxy-unaware clients fail closed.
- **A compatibility window for pre-release schemas.** A database written
  by an unknown build is refused with instructions, not repaired.

## Where the escalation goes

See [SUPPORT.md](../../SUPPORT.md) for where to ask, and
[SECURITY.md](../../SECURITY.md) for how to report a vulnerability —
never a public issue.
