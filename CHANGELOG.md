# Changelog

Notable changes, newest first. Runpool follows
[Semantic Versioning](https://semver.org/): the compatibility surfaces
a version speaks for are listed in
[the product contract](docs/product-contract.md).

## Unreleased

**Nothing has been released.** There is no tag, no published binary and
no image. The first release is `v1.0.0`, and it is
blocked on the external security review and release qualification on
the reference host — see
[release readiness](docs/release-readiness.md) for the live status and
for exactly what unblocks each.

This section records the product delivered by the first release, grouped
by its public and operational effects.

### Isolation and egress

- A job runs in **one capsule**: supervisor, runner and the job's own
  Docker daemon in a single container under one aggregate cgroup.
- **The egress gateway always runs the capsule this build ships.**
  Extending the image a tier's jobs run in does not replace the container
  that applies the network policy confining them.
- **A tier may name its own capsule image.** `tiers[].capsuleImage` takes
  a digest-qualified image built from the published capsule, so a
  deployment can add packages and toolchains to its jobs without a fork.
  The controller reads the protocol version the capsule declares before
  handing it anything and refuses one it does not speak; that attempt is
  held for review as `capsule_incompatible` rather than retried, because
  the next attempt would launch the same image. `runpool status` reports the image each tier runs, because a
  replaced capsule is outside the configuration the release gates
  observed.
- Under the restricted network profile a capsule has **no route out**.
  Its only egress is a per-capsule gateway that resolves and connects
  on its behalf under a default-deny policy, which is also the DNS
  rebinding defence.
- **The gateway is inside the lease's budget.** The tier is split
  between capsule and gateway under one parent cgroup, so the two
  together are the tier and never more.
- **Policy changes are classified.** A restriction that cannot be
  installed closes the affected gateway; discovery failing at all
  closes every gateway. The previous policy is never treated as a safe
  fallback.
- The relay's CONNECT port set, header handling, parser strictness and
  every concurrency and size bound are explicit, tested and fuzzed.
- **Containers a job starts take the same relay.** A daemon does not pass
  its own environment to what it runs, so the capsule's proxy did not
  reach a `docker build` or a `docker run` the job issued — and under the
  restricted profile those have no route out either. The supervisor
  writes the proxy into the runner's Docker client configuration, which
  is where the client injects it into every container and build it
  creates.
- **An operator allowance is bounded by what it reopens.**
  `network.allowPrivateCIDRs` punches holes through the built-in deny
  set, so an entry *broader* than a withheld range would reopen that
  whole range as a side effect; the validator refuses exactly those.
  Public prefixes are accepted because the profile already permits the
  public internet — which is how a capsule reaches a service on the
  host's own public address, an address the runtime deny set withholds
  from facts static validation cannot see.

### State and operations

- **A deployment authenticates as itself, not as a person.** A credential
  is a personal access token or an installation of a GitHub App;
  `github_app` takes a client id, an installation id and a PEM key, and
  the provider client mints and refreshes the installation token on its
  own. An App credential belongs to the organization, so membership
  changes stop being an outage. A secret file other local users can read
  is refused for either type, and `runpool doctor` reports which identity
  it authenticated as.
- Host ownership is explicit: **`shared-daemon`** supports Dokploy-style
  coexistence with positive host reserves, restricted egress, runner-group
  isolation, ownership-verified destructive intents, and idle-uplink recovery;
  **`dedicated-daemon`** remains the smaller-blast-radius option.
- The schema ships as **one reviewed baseline**. After the first release,
  migrations are forward-only and immutable.
- **The attempt owns execution evidence and disposition**; a
  `capsule_lease` owns only the host resources it consumes, and carries
  no provider identifier. GitHub's scale sets, runner ids and workflow
  runs live in the adapter's own tables.
- **There are no down migrations.** Restoring the pre-migration backup
  is the rollback, because a down script claims every schema change is
  losslessly reversible and a dropped column is not.
- **A schema is identified by its contents.** The applied migrations'
  fingerprint is recorded with the schema, in the same transaction, and
  checked on every open. A version counter cannot tell two schemas apart
  while the reviewed baseline is still edited in place, which is exactly
  when a database written by an earlier build would otherwise be accepted
  and then fail on a missing table.
- **A schema this build cannot account for is refused, not repaired** —
  by reporting as well as by the controller. `status`, a `gc` dry run and
  a `cleanup` or `uninstall` preview apply no migrations, so each says
  what it found and how to recover: a newer schema names the build that
  wrote it, an older one says to start the controller that migrates it.
- **A restart is not a provider dependency.** Startup reconciliation
  adopts running capsules and resolves interrupted leases before the
  first call to the provider. A binding whose scale set or message
  session cannot be reached retries on its own loop, so an outage or an
  expired token costs that binding its turn rather than ending the
  process while capsules are still running. The shutdown drain is sized
  to end inside the deployment's stop grace period, so the message
  sessions are closed rather than left for the next start to wait out.

- Cache lanes are **daemon-side named volumes**, exclusive per lease,
  reused only for the same repository and generation, garbage-collected
  by TTL and LRU under disk pressure. A soft emergency sweeps every free
  lane instead of working down to a byte watermark, so lanes the daemon
  cannot size are reclaimed with the rest, and those evictions carry
  their own reason in the audit log.
- A **disk-pressure state machine** closes admission before the host
  fills, with hysteretic recovery. Free bytes and free inodes are both
  measured and both get a recovery band, so neither dimension flaps at
  its boundary; a filesystem with nothing left at all reads as an
  emergency rather than as a failed measurement.
- **Finished lease records are forgotten on a window.**
  `retention.leaseHistory` defaults to 90 days — long enough to explain
  an incident from last quarter — and zero keeps every record. The
  window is measured from when a lease finished. A bounded pass runs
  with the periodic reconciler and `runpool gc` does the same on demand,
  previewing by default. It never touches the attempt: what the work did
  is the record, and a lease is the runtime plumbing that served it. A
  lease whose attempt is still open, or that still owns a resource
  intent, is never forgotten — either would hide live work.
- **Three failures that said nothing now say something.** A `--json`
  command answers with a document before the instance has ever run,
  rather than a line of prose and a success exit code. A cgroup driver
  this build cannot write a parent for fails the host contract, rather
  than launching capsules whose tier quietly stops being the sum of the
  capsule and its gateway. And a Quick Start variable set beside a
  configuration file is refused rather than ignored.
- **What is qualified is stated apart from what is accepted.** The
  release gates observe `github.com` at repository and organization scope
  on `linux/amd64`. Enterprise Server, data-residency hosts, enterprise
  scope, other architectures and an operator-supplied capsule are
  accepted and unqualified — no gate has observed them, which is neither
  a promise nor a denial. GPU and device passthrough are not implemented
  at all, and the support matrix says which is which.
- **The workflow side is documented.** A workflow reaches a tier by naming
  its scale set — `runs-on: runpool-<tier id>` by default, not a label
  set — and the deployment guide states what the GitHub side has to be
  true for work to arrive at all. `deploy/workflows/example.yml` is a
  complete job, linted with the rest.
- **`runpool attempts inspect` reports what a resolution turns on.** The
  evidence rung and the provider's own identifiers for the run, so the
  external check the command's help prescribes can be carried out from
  the tool that prescribes it.
- **A retry that repeats is bounded.** An attempt whose work provably
  never began is served up to three times in all; past that it goes to
  manual review as `retry_budget_exhausted` rather than burning a capsule
  per pass forever. An operator resolving that review is not overruled by
  the counter. `scheduling.retryBudget` moves the number within `1..10` —
  a narrow range, because this breaks a loop rather than tunes a rate.
- **A target is any host the protocol serves, at any scope it defines.**
  The URL rule refused every host but `github.com`, which also refused an
  Enterprise Server and a data-residency host that speak the same
  endpoints — and it masked a defect, since `/enterprises/<name>` passed
  that rule and was then read as a repository named after the enterprise.
  Enterprise is now a scope of its own, carrying the runner-group rule an
  organization carries; cache lanes stay repository-only. Whether a host
  answers is what `runpool doctor` proves, and what has been qualified is
  a separate statement in the support matrix.
- **A job ceiling per tier.** `tiers[].jobTimeout` bounds how long Runpool
  waits for a capsule that stopped reporting. It defaults to `8h`, above
  the provider's own 360-minute maximum for a job, because a backstop
  below that ends work the provider still permits and one equal to it
  races the provider's own timeout. Expiry says which tier's ceiling
  ended the capsule.
- **Uninstall takes the cache lanes with it.** Their volumes are removed
  and their rows purged, so a reinstall does not inherit a ceiling
  consumed by lanes nothing can reach.
- **Capacity is credit**: a tier advertises no more than it has, and a
  rotating discovery credit keeps a quiet binding from going blind to
  its own queue.
- **Scheduling is explicit at two levels.** `tiers[].parallelism` limits
  active leases per resource tier; the optional `scheduling.parallelism`
  limits them across the whole instance, constraining both what the
  controller advertises to the provider and what it admits locally.
  Omitting the global field keeps tiers independent; zero is invalid
  rather than overloaded to mean unlimited. A lease counts from durable
  reservation until cleanup reaches `released`, and startup
  reconciliation adopts existing leases before admitting new work.
- **Resource envelopes are provider neutral**: `cpu`, `memory`, `swap`
  and `pids`. `swap` is additional swap above the memory limit, not
  Docker's combined memory-plus-swap total — the adapter derives that —
  and Runpool never requests an unlimited value. `host.reserve.swap`
  withholds host swap from scheduling. Preflight proves the worst
  admitted CPU, memory and swap set plus the reserve fits the host, and
  configured tier swap additionally requires cgroup swap enforcement.
- Swap is an emergency buffer, not ordinary capacity: production
  guidance recommends encrypted persistent swap where CI secrets may
  reach memory.
- **Pre-migration backups are never overwritten**, and a database from
  an unknown build is refused with instructions rather than repaired.
- Install, backup, restore, upgrade and uninstall are **executed** by
  the lifecycle drills, not merely documented.
- The lease machine, the cleanup saga and disposition-by-evidence live
  in **`internal/lease`**, with their own tests. They know nothing about
  providers, which is what keeps cleanup working when one is
  unreachable.

### Interface

- The CLI is **Cobra**: `--help` works, extra arguments are usage
  errors, exit codes are `0`/`1`/`2` and mean what they say, and
  destructive commands preview by default.
- **Cobra is the only parser.** Command bodies take typed parameters
  and return errors; nothing re-parses a flag a second time, so the
  generated reference and the behaviour cannot describe different flag
  sets.
- `status --json` is a **versioned reporting document** (`v1`),
  and its books-versus-daemon comparison covers containers, networks
  and volumes. It also reports the effective host topology and the
  scheduling picture: mode, configured and effective parallelism,
  active and available capacity, queue depth, and per-tier accounting.
- **A binding reports what it reaches.** Each one carries when a provider
  call for it last succeeded and what it cannot do now, so an instance
  with no work to do is distinguishable from one reaching nothing —
  which every other field in the document reads identically. The record
  is durable, so the answer survives the controller that stopped being
  able to reach anything.
- **`runpool doctor` names the label workflows target.** For each scale
  set a deployment serves it reports the provider id where one exists and
  the `runs-on` value that reaches it, so the string in configuration and
  the string in a workflow can be compared without reading either.
- **The document is bounded by live work, not by history.** Its `leases`
  array carries every unreleased lease without exception — that set is
  what the instance is still responsible for — plus the finished ones
  that finished most recently. `released_total` reports how many
  finished leases exist, because the array's length is what was reported
  and not what the store holds.
- The CLI reference and shell completions are **generated from the
  command tree**.

### Release qualification and gates

- A release qualifies the exact digest-qualified controller and capsule
  candidates it later promotes; no image is rebuilt after qualification.
- Platform qualification compares every locked host fact and fails when a
  value is missing, not only when a reported subset differs.
- The standalone binary and completions are built once before qualification,
  retained by checksum, and published without rebuilding. Releases also carry
  separate controller and capsule SBOMs plus signed provenance attestations.
- Public-repository gates include CodeQL, dependency review, Dependabot,
  vulnerability scanning, SHA-pinned actions, and least-privilege workflow
  permissions.
- The qualification policy is **pending in `build/platform.lock.json`**.
  Docker Engine 29.7.2 on Debian 13 is selected for the first qualification;
  the exact host facts must be captured, reviewed, and frozen before a release
  candidate is authorized. Contract suites fail closed while the lock is
  pending and later compare the host against the frozen manifest.
- Release qualification runs its live suites **on the reference host** and
  builds its record from evidence emitted there.
- No contract may be skipped in release-qualification mode.
- The controller E2E drives three real assignments through the exact image
  candidates: restart recovery, cache reuse, generation isolation, restricted
  egress, inner Docker build and registry push, remote cleanup, and preservation
  of unrelated container, network and volume sentinels by exact id.
