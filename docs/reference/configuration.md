# Configuration reference

Two ways in, and they do not mix.

**Quick Start** is environment variables and nothing else. It builds a
complete, validated configuration for the common case: one target, one
tier, no cache.

**File mode** is `RUNPOOL_CONFIG_FILE` pointing at a YAML document.
Setting it together with any Quick Start target variable is an error,
not a merge — a configuration assembled from two sources is one nobody
can read back.

Whichever you use, the validator is the authority on what is accepted,
and `runpool config effective` prints the defaulted, validated result.
Run it before you run anything else: it is the only view that shows what
the controller will actually do, defaults included.

```bash
RUNPOOL_GITHUB_URL=https://github.com/acme/app \
RUNPOOL_GITHUB_TOKEN=… \
RUNPOOL_HOST_TOPOLOGY=shared-daemon \
RUNPOOL_HOST_RESERVE_CPU=1 \
RUNPOOL_HOST_RESERVE_MEMORY=2GiB \
RUNPOOL_HOST_RESERVE_SWAP=0B \
RUNPOOL_HOST_RESERVE_FREE_DISK=20GiB \
runpool config effective
```

## Quick Start variables

| Variable | Required | Meaning |
| --- | --- | --- |
| `RUNPOOL_GITHUB_URL` | yes | `https://github.com/<owner>` or `https://github.com/<owner>/<repo>`. The scope decides what the runner may bind. |
| `RUNPOOL_GITHUB_TOKEN` | one of | The token, read from the environment at startup. |
| `RUNPOOL_GITHUB_TOKEN_FILE` | one of | A file to read the token from. Setting both is an error. |
| `RUNPOOL_GITHUB_RUNNER_GROUP` | conditional | Required for an organization target in `shared-daemon`; invalid for a repository target. |
| `RUNPOOL_HOST_TOPOLOGY` | yes | `shared-daemon` or `dedicated-daemon`; the compromise and cleanup model must be explicit. |
| `RUNPOOL_HOST_RESERVE_CPU` | shared | CPU withheld from scheduling. Required and greater than zero in shared mode; dedicated default `1`. |
| `RUNPOOL_HOST_RESERVE_MEMORY` | shared | Memory withheld from scheduling. Required and greater than zero in shared mode; dedicated default `2GiB`. |
| `RUNPOOL_HOST_RESERVE_SWAP` | no | Host swap withheld from scheduling. Default `0B`. |
| `RUNPOOL_HOST_RESERVE_FREE_DISK` | shared | Free disk floor for platform and host. Required and greater than zero in shared mode; dedicated default `20GiB`. |
| `RUNPOOL_TIER` | no | The tier id. Default `standard`. |
| `RUNPOOL_PARALLELISM` | no | Maximum active leases in the Quick Start tier. Default `1`. |
| `RUNPOOL_LOG_LEVEL` | no | `debug`, `info`, `warn` or `error`. Default `info`. |
| `RUNPOOL_NETWORK_PROFILE` | no | `public-internet-only` (default) or `unsafe-open-egress`. |
| `RUNPOOL_CONFIG_FILE` | no | Switches to file mode. Mutually exclusive with **every** variable above: in file mode each of those settings comes from the file, and one that is neither applied nor refused is worse than either. |

`RUNPOOL_STATE_DIR` selects the state directory and is read in both modes,
by `serve` and by every command that reads or maintains state. It defaults
to `/var/lib/runpool/state`, which is where the reference deployment
mounts the state volume — so a command run outside that container reports
an instance that has not run yet until it is pointed at the real
directory.

Quick Start leaves the cache **off**. A lane is durable state that outlives
the job that filled it and is shared by every later job for its repository,
which is a decision an operator makes rather than one a default makes for
them.

## The document

`apiVersion: runpool.rhobuild.com/v1`, `kind: RunpoolConfig`.
Unknown fields are rejected by name rather than ignored — a typo that
silently does nothing is worse than one that stops startup.

The version is the **configuration schema's**, not the product's. The
two move independently, and neither is promoted while its contract is
still moving.

### instance

`name` is a lowercase slug, default `primary`. It appears in logs and
distinguishes co-located instances in a human's reading, not in
ownership: ownership is the opaque instance id in the state directory.

### host

`topology` is required:

| Value | Meaning |
| --- | --- |
| `shared-daemon` | Runpool shares the Engine with a platform such as Dokploy and its application services. Restricted egress and a positive explicit reserve are mandatory. |
| `dedicated-daemon` | The Engine is operationally exclusive to Runpool. Default reserves are CPU `1`, memory `2GiB`, swap `0B`, free disk `20GiB`. |

`reserve.cpu`, `reserve.memory`, `reserve.swap`, and `reserve.freeDisk` are
capacity Runpool does not schedule. `freeDisk` also feeds disk-pressure
admission. Shared mode requires CPU, memory, and free disk to be greater than
zero because Runpool cannot infer the peak needs of colocated services. Swap
may remain `0B`. These controls reduce resource contention; they do not isolate
controller, daemon, or kernel compromise.

### scheduling

`scheduling.parallelism` is an optional instance-wide limit across every
target and tier. When present, it constrains both provider capacity advertised
by the controller and local admission. It must be between `1` and `10000` and
must not exceed the sum of tier parallelism. A value of `0` is invalid.

When omitted, tiers are deliberately independent and the effective maximum is
the sum of `tiers[].parallelism`. This preserves a useful multi-tier default
without hiding a global policy. Use an explicit global value on a shared host
where one large workload or several smaller workloads must stay inside a
single host budget.

`scheduling.retryBudget` is how many times one attempt whose work
provably never began may be served before it is held for review as
`retry_budget_exhausted`. It defaults to `3`, which matches the
provider's own budget for an assignment nobody takes, and may be set
between `1` and `10`.

The range is narrow on purpose. This breaks a loop rather than tunes a
rate: a serving that ends before the work begins is a transient — an
image that would not pull, a daemon that would not answer — and the
failure that repeats is the one no number of retries fixes, while each
retry costs a capsule, a runner registration and a lane. Raising it far
is how a systematic failure gets paid for instead of found. An operator
resolving a held attempt is not overruled by the counter.

A lease counts from durable reservation through provisioning, execution,
draining, cleanup, failure, and quarantine. Capacity returns only when cleanup
reaches `released`. Startup reconciliation adopts existing leases before new
work is admitted, so a controller restart cannot reset either the tier or
instance-wide count.

### tiers

A tier is a named resource envelope with its own parallelism limit.

| Field | Meaning |
| --- | --- |
| `id` | Lowercase slug; targets reference it |
| `parallelism` | Maximum active leases in the tier, `1..10000`; default `1` |
| `resources.cpu` | CPU limit, `> 0`; default `2` |
| `resources.memory` | Memory limit, `> 0`; default `4GiB` |
| `resources.swap` | Additional swap above memory, `>= 0B`; default `0B` |
| `resources.pids` | Process ceiling, `>= 1`; default `1024` |
| `capsuleImage` | Capsule image for this tier's jobs, digest-qualified; default is the image this build ships |
| `jobTimeout` | How long Runpool waits for a capsule that stopped reporting, `>= 1m`; default `8h` |

**The envelope covers the capsule and its egress gateway together.** The
gateway takes a fixed reserve out of it, so a tier whose resources are
smaller than that reserve plus a usable capsule is rejected at startup
with the exact figures — better than a job that starves at run time
because something invisible was charged to it.

#### The job ceiling

`jobTimeout` is not the job's time limit. GitHub ends a job at its own
`timeout-minutes` — 360 minutes at most — the runner exits, and the lease
resolves through the ordinary path. The ceiling is what bounds a capsule
that stops reporting instead: a wedged process, a daemon that will not
stop, a capsule that lost the ability to say anything. Without it, that
lease would hold its credit and its privileged container indefinitely.

So it belongs above the largest legitimate run, not below it. The default
of `8h` leaves the provider's own maximum two hours to fire, reach the
runner, exit it and be observed here, and `168h` is the most a tier may
set — a wedged capsule holds a lane, an admission credit and a cache lane
for the whole ceiling, so an unbounded value is how that becomes
permanent by configuration. Lower it on a tier whose work is short to get
its capacity back sooner; the log says which tier's ceiling ended a
capsule, so it is never mistaken for a job that failed.

**It bounds the wait for the job, and nothing else.** Getting a capsule
to the point of running — minting a credential, acquiring a lane,
building the sandbox, pulling and booting the image, handing over the
credential — is this instance's own work against its own daemon and is
bounded separately. That is why the floor can be as low as `1m` without
ending capsules while they are still starting, and why an adopted capsule
waits out what its lease has left rather than starting a fresh ceiling on
every restart.

#### The capsule image

The capsule is not an arbitrary container. It is the other half of a
control protocol: its entrypoint is the Runpool supervisor, which owns
PID 1, boots the job's Docker daemon, answers `deliver`, `start` and
`state` over `docker exec`, and writes the protocol version it speaks to
`/run/runpool/protocol` at boot. The controller reads that file before it
hands a capsule anything and refuses a version it does not speak. A
mismatch holds that attempt for review as `capsule_incompatible` rather
than retrying it: the next attempt would launch the same image and meet
the same answer, so what a retry buys is the same fact three times.

An operator's image therefore derives from the published one:

```dockerfile
FROM ghcr.io/rhobuild/runpool/capsule@sha256:<digest>
RUN apt-get update && apt-get install -y --no-install-recommends <packages> \
 && rm -rf /var/lib/apt/lists/*
```

**Do not end it with a `USER` directive.** The published image is root on
purpose: the supervisor is PID 1 and needs root to boot the inner Docker
daemon, and it drops the runner to uid 1001 itself once the job is handed
over — which the image's last `USER` cannot say. A derived image that
ends as another user cannot write its own control surface, so it cannot
report why it stopped either: every attempt on that tier is held as
`capsule_incompatible`, which reads as a version mismatch and sends you
back to a digest that was right.

The supervisor, its entrypoint, and the `/run/runpool` control surface are
the parts that may not be replaced. Everything else — packages, language
toolchains, preinstalled software — is yours.

`capsuleImage` must name a digest. The controller launching an image it
can name exactly is the property the shipped pin provides, and a tag can
move under a running controller; the validator refuses one rather than
letting a deployment discover it later.

**The egress gateway is not this image.** Each lease runs a gateway
container that applies the network policy, and it always runs the capsule
this build ships — extending the image a tier's jobs run in is not a
request to replace what confines them.

**A tier that names its own capsule is outside the configuration the
release gates observed.** Nothing about that is unsafe — a deployment
already holds the host's Docker socket, so its operator can run any image
against that daemon directly — but it is not the configuration any
qualification result speaks for. `runpool status` reports the image each
tier runs, so the deviation is visible rather than inferred.

`resources.memory` is the RAM hard limit. `resources.swap` is additional swap,
not Docker's combined memory-plus-swap value. Runpool performs that adapter
translation internally and never requests unlimited swap.

`runpool doctor` gates swap on two separate conditions, because they ask
different questions:

- **When any tier uses swap**, Docker must be able to enforce a memory-swap
  limit — cgroup v2 swap accounting. This keys on tier swap alone: a limit is
  only ever set on a container, and `host.reserve.swap` never reaches one.
- **When any tier swap or `host.reserve.swap` is non-zero**, the host swap
  total must be readable and large enough for the conservative workload set
  plus the reserve. Reserving swap the host does not have is a sizing error
  whether or not a capsule would use it.

On hosts that may contain CI secrets, persistent swap should be encrypted.
Treat swap as an emergency buffer, not as normal execution capacity: sustained
swap use is a sizing signal and can make CI dramatically slower.

The capacity preflight is conservative. Without an instance-wide limit it
budgets every tier at full parallelism. With a global limit of N it selects the
N largest eligible envelopes independently for CPU, memory, and swap, then
adds the host reserve. For `parallelism: 1`, that means the largest tier—not
the sum of all tiers—must fit.

### targets

| Field | Meaning |
| --- | --- |
| `id` | Lowercase slug |
| `url` | The target: `https://<host>/<owner>`, `https://<host>/<owner>/<repository>` or `https://<host>/enterprises/<name>` |
| `credential` | Credential id from `credentials[]` used to authenticate the target |
| `runnerGroup` | Runner group. Required for organization and enterprise targets in shared mode; invalid for repository targets |
| `cache.enabled` | Persistent cache lanes for this target |
| `cache.generation` | Lowercase slug; changing it abandons the old lanes |
| `tiers[].tier` | Tier id from `tiers[]` to serve with |
| `tiers[].scaleSetName` | The scale set name on GitHub, lowercase slug. **This is the `runs-on` value** a workflow uses; defaults to `runpool-<tier id>` |

**A persistent cache requires a repository-scoped target.** A runner that
is not bound to one repository could execute another repository's job
against its cache. The validator refuses the combination rather than
trusting the operator to remember.

**The host is carried, not checked.** Whether a host serves the protocol
is a question the provider answers, and `runpool doctor` asks it with a
real call before any work is served. Refusing an unfamiliar name here
would refuse an Enterprise Server or a data-residency host that speaks
exactly the endpoints `github.com` speaks. What has been *qualified* is a
separate statement — see the [support matrix](support-matrix.md).

The same `scaleSetName` in different GitHub scopes names different
resources and is accepted.

**A workflow reaches a tier by naming its scale set**, not by matching
labels:

```yaml
jobs:
  build:
    runs-on: runpool-standard
```

`runs-on: [self-hosted, linux, x64]` does not reach one. `runpool doctor`
prints the name for every tier a deployment serves. The complete example
is [`deploy/workflows/example.yml`](../../deploy/workflows/example.yml).

### credentials

| Field | Meaning |
| --- | --- |
| `id` | Lowercase slug |
| `type` | `token` or `github_app` |
| `tokenEnv` | Environment variable holding the token, for `token` |
| `tokenFile` | File holding the token, for `token` |
| `clientID` | The App's client id, for `github_app` |
| `installationID` | The installation this deployment acts as, for `github_app` |
| `privateKeyEnv` | Environment variable holding the App's PEM key |
| `privateKeyFile` | File holding the App's PEM key |

The configuration never contains the secret itself — it names where to
find it, so a configuration dump is safe to paste into an issue. Exactly
one reference per credential: `tokenEnv` or `tokenFile`, `privateKeyEnv`
or `privateKeyFile`. A credential carrying the fields of both types is
refused rather than resolved by precedence.

Prefer the file forms for deployments, so the credential is mounted as a
secret rather than persisted in Docker's container environment. A secret
file other local users can read is refused: `shared-daemon` is a
supported topology, so another uid on the host is a real party.

Runpool reads the credential at startup; rotating it requires a
controlled controller restart. Use `runpool doctor` to verify the target
before serving work — it reports which identity it authenticated as.

#### Which type

**`token`** is a personal access token. It carries the permissions of the
person who minted it, appears in their account, and stops working when
they rotate it or leave. Grant only the target's self-hosted runner
administration permissions.

**`github_app`** authenticates as an installation of a GitHub App, which
belongs to the organization rather than to a person. It is the right
credential for anything long-running: membership changes stop being an
outage, and the installation's scope replaces a person's permissions.
Runpool hands the client id, the installation id and the key to the
provider client, which mints the installation token and refreshes it
before expiry on its own.

The private key is the longest-lived secret a deployment holds, and the
one whose leak revoking a single token cannot contain. Mount it as a
file, owner-readable only.

```yaml
credentials:
  - id: runners
    type: github_app
    clientID: Iv1.0123456789abcdef
    installationID: 12345678
    privateKeyFile: /run/secrets/runpool/app.pem
```

### cache

`storage.mode` is `volume`. No other mode is implemented, and the validator
refuses one rather than accepting a value that does nothing.

| Field | Meaning |
| --- | --- |
| `global.maxManagedBytes` | The cache budget the watermarks are a fraction of; default `150GiB` |
| `global.highWatermarkPercent` | Where collection starts, `1 <= low < high <= 99`; default `80` |
| `global.lowWatermarkPercent` | What collection collects down to; default `65` |
| `global.softEmergencyFreeBytes` | Free space at which admission closes; default `20GiB` |
| `global.hardEmergencyFreeBytes` | Free space at which everything fails closed; must be below the soft threshold; default `10GiB` |
| `defaults.repositoryMaxBytes` | Per-project ceiling; may not exceed the global budget; default `15GiB` |
| `defaults.unusedTTL` | How long a free lane survives unused, `> 0`; default `720h` |

What each pressure level means operationally is in
[the runbook](../runbook.md).

### retention

How long durable records outlive the work they describe.

| Field | Meaning |
| --- | --- |
| `leaseHistory` | How long the record of a finished lease is kept. Default `2160h` (90 days); `0` keeps every one; otherwise at least `24h` |

A `capsule_lease` is the runtime plumbing an attempt consumed — the
containers, networks and volumes it held. **What the work did is the
attempt's evidence, and is never pruned**: its disposition and its
lifecycle events outlive the lease that produced them.

Unlike `scheduling`, this section always appears in `config effective`
even when the file omits it. A deletion policy that applies by default
should not be invisible.

The serving controller prunes on its reconcile interval, and `runpool gc`
does the same pass on demand. A lease is skipped while the attempt it
served is still unresolved, or while it still owns a resource intent —
the first keeps a crashed-mid-release attempt findable, the second keeps
a real leak visible.

### observability

`log.format` is `json` or `text`; `log.level` is `debug`, `info`, `warn`
or `error`.

`metrics.enabled` **must be false**, and this is settled rather than
pending. Runpool exposes no metrics endpoint and will not at this scope:
`runpool status --json` is the machine-readable account, versioned by
its own `api_version`, and the host decides when a person has to look —
[the runbook](../runbook.md) shows what to evaluate, and
[the decision record](../adrs/2026-08-28-the-status-document-is-the-metrics-interface.md)
says what was rejected and why. The field is still accepted because
configuration parsing is strict and the shipped example writes it, so
removing it would fail the startup of every deployment that copied that
example. Removing it would be a major-version break, which is a
classification and not a plan: nothing schedules it, and this reference
will say so if anything ever does. A scrapeable
surface, if one is ever justified, would be a new field rather than this
one changing meaning.

### network

| Field | Accepted | Meaning |
| --- | --- | --- |
| `profile` | `public-internet-only` | The restricted profile: no route out, egress through the policy relay |
| | `unsafe-open-egress` | Capsules reach whatever the host reaches, including the LAN. Logged loudly at startup |
| `ipv6` | `disabled` | The sandbox denies IPv6 and the validator refuses a value claiming otherwise |
| `dns.mode` | `gateway` | Names are resolved by the gateway that then dials them — which is the DNS-rebinding defence |
| `allowPrivateCIDRs` | list | Private ranges a capsule may reach anyway. IPv4 only, and no entry may be broader than a range the built-in deny set withholds |
| `denyCIDRs` | list | Ranges denied on top of the built-in set. IPv4 only |

`allowPrivateCIDRs` punches holes through the built-in deny set, and an allow
is consulted before a deny when a destination is decided. An entry that is
*broader* than a withheld range therefore reopens that whole range as a side
effect — `0.0.0.0/0` readmits every private network while `profile` still
reads `public-internet-only` — so the validator refuses any entry that
strictly contains a denied range.

An entry that names something no connection reaches is refused for a
different reason: it cannot take effect. Loopback is the gateway itself, and
multicast, broadcast and the unspecified address are not destinations a relay
connects to. Accepting one would put an allow rule in the rendered firewall
while the gateway refused every request through it, with nothing anywhere
saying why.

Link-local is not in that group. It is denied by default — a cloud instance
keeps its own credentials there, and a job that wanders into it should not
arrive — and a deployment with a reason to reach one address in it names that
address, as a single `/32`. Anything wider is refused: not by the rule above,
which only catches an entry *broader* than a withheld range, but by a rule of
its own, because the baseline withholds link-local as a `/16` and an entry of
exactly that would be broader than nothing.

Narrower entries are the intended use, and public prefixes are accepted
because they change nothing: the profile already permits the public internet.
That is also how a capsule reaches a service on the host's own public
address, which the runtime deny set withholds from facts static validation
cannot see. Both lists render into an IPv4 ruleset; an IPv6 prefix is refused
here rather than at gateway boot.

Under the restricted profile, direct egress is denied. Proxy-aware clients
may use HTTP or CONNECT to allowed addresses on ports 80 and 443; CONNECT is
opaque. `git+ssh`, other ports, non-DNS UDP, and proxy-unaware clients fail
closed; see
[the product contract](../product-contract.md) for what that means for a
workflow.

`shared-daemon` rejects `unsafe-open-egress`: permitting a capsule to reach
the host and private networks would contradict the coexistence contract. Move
that workload to `dedicated-daemon` if it cannot use the restricted relay.

## Where the rules actually live

Every constraint above is enforced by `internal/config`, and the error
messages name the field path and the reason. This page describes them;
it does not define them. Where the two disagree, the validator is right
and this page is a bug.
