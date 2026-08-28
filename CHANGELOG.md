# Changelog

Notable changes, newest first. Runpool follows
[Semantic Versioning](https://semver.org/): the compatibility surfaces
a version speaks for are listed in
[the product contract](docs/product-contract.md).

## Unreleased

### Operations

- **The absence of a metrics endpoint is a decision, and the runbook now
  says what to watch instead.** `observability.metrics.enabled` was refused
  with the word "yet", which is a promise nobody had a date for. There is no
  endpoint and there will not be one at this scope: `runpool status --json`
  is the machine-readable account, versioned for exactly this, and deciding
  when a person has to look belongs to the host. The runbook's new "Watching
  an instance" names the six conditions that need one and gives a command
  that prints a line per condition and nothing when there is nothing to look
  at. The field is still accepted — configuration parsing is strict and the
  shipped example writes it, so removing it would fail the startup of every
  deployment that copied that example — and is planned for removal in a major
  version. See
  [the decision record](docs/adrs/2026-08-28-the-status-document-is-the-metrics-interface.md).

## v1.1.0 — 2026-08-28

A capsule adopted across a controller replacement is read under its own
protocol, a held attempt is resolved without stopping the controller, and a
registry that blinked no longer costs an attempt. The schema is unchanged, so
an installation moves by repointing the image and recreating the container;
[the runbook](docs/runbook.md) names the commands and what to do if it has to
go back.

### Isolation and execution

- **A capsule adopted across a controller replacement is read under the
  protocol it declares, not the one this build speaks.** Both bumps this
  protocol has had moved what an existing word means, and a word read under
  the wrong version is understood rather than refused — `waiting` from an
  older supervisor reads as proof the runner never started, and returns to
  the queue an assignment its capsule is forking a runner for. A capsule
  whose protocol this controller does not speak now holds its attempt for a
  person. Its exit code is still trusted from every version, and is frozen:
  it is the only account a stopped capsule leaves. See
  [the ADR](docs/adrs/2026-08-27-an-adopted-capsule-is-read-in-its-own-dictionary.md).

### Operations

- **Resolving a held attempt no longer stops the controller.** `runpool
  attempts resolve --apply` hands the decision to the controller serving
  that state directory, which applies it; with none running it writes
  directly under the lock as before. One command either way. Every write
  still belongs to whoever holds the lock — the decision travels to it
  rather than around it — and the audit record names the operator who
  decided, never the controller that carried it. See
  [the ADR](docs/adrs/2026-08-27-the-resolution-reaches-the-writer.md).
- A pull the registry refused in a way it might not repeat is tried again,
  up to three times, instead of spending an attempt's retry budget on a
  blip. An answer that will not change — an unpublished reference, a
  credential that does not carry, a malformed reference — is not retried.

## v1.0.0 — 2026-08-27

The first release. It publishes only what its qualification proved for this
exact commit and these exact image digests: the live and end-to-end suites on
the reference host, the upstream provider contracts against real fixtures, and
the artifact checksums. The gates it must clear, and the evidence each one
leaves, are in [release readiness](docs/release-readiness.md).

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
  the next attempt would launch the same image. That version is read
  before readiness is waited on, so an image that is not this build's
  half of the protocol is refused in about a second rather than after a
  readiness deadline that was never going to be met. `runpool status`
  reports the image each tier runs, because a replaced capsule is outside
  the configuration the release gates observed.
- **A prepared capsule has a daemon that answers.** The state the
  controller delivers a credential on is written by the supervisor only
  once the job's own Docker daemon is up, so readiness is something the
  capsule proves rather than something it announces on the way to
  proving it.
- **An assignment is requeued only on proof that no runner ever
  started.** A capsule that has accepted a start authorization says so
  before the authorization lands, and keeps saying it until the runner is
  forked or the attempt is abandoned. Without that the whole preamble —
  reading the credential bundle, materializing it, removing it, forking —
  answered with the same state as a capsule holding an unstarted runner,
  so an authorization whose call returned an error after taking effect
  would put the assignment back in the queue while the capsule was
  handing it to a runner, and it would run twice. The controller now
  holds such an attempt for a person, because at that moment neither
  answer is available — and an authorization that could not be written at
  all says so, so an assignment nothing ever started is still simply
  served again.
  That proof outlives the pass that took it. Cleanup can fail against a
  wedged daemon, and the retry is a different pass with nothing measured:
  it either measured nothing or found the capsule a measurement would
  have come from already gone, which is the one this cleanup is removing.
  What a serving establishes is kept with its lease, so the retry reads it
  back rather than settling a job that never ran as one that did. A later
  measurement that does establish something still wins, because the
  accounts arrive in order — the daemon's, then the capsule's own, then
  the provider's.
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
  fallback. A policy counts as in force only once it has reached every
  gateway that could be named, so a pass that could not name one leaves
  the books where they were and the next pass installs the change
  instead of comparing it against itself.
- **A revoked address stops being reachable, transfers under way
  included.** A destination the policy no longer allows is checked when a
  connection is dialled, and the pool a job keeps warm never dials again
  — the kernel does not intervene either, since the ruleset accepts
  established traffic ahead of every reject. A policy that moves
  therefore retires the pool it authorised rather than closing the
  connections that happen to be idle in it. Retiring the pool does not
  reach a transfer already running: the connection carrying it is left
  alone, and a large download can hold one for as long as the job likes.
  So both shapes the relay serves re-check the address they are joined to
  while they run — a CONNECT tunnel and a plain HTTP transfer alike — and
  the longest a revoked destination keeps flowing is one poll interval,
  whatever it is carrying. A transfer stopped that way reaches the job as
  a failed read rather than as a complete response with a short body:
  once the status line is out there is nothing left to say "this was
  cut" except how the connection ends, so it ends without the terminator
  a client would otherwise read as the end of the data.
- **An install takes effect whenever it lands.** Whether a new policy is
  in force is decided by the document's contents rather than by its
  modification time and size. Two documents of equal length installed
  inside one clock tick carry the same modification time, so deciding on
  that pair meant the second never reached the relay at all: the
  tightening would have been reported as installed and silently not
  applied. A document larger than a policy may be is refused rather than
  cut down to the limit, since a cut one can still parse as a policy
  nobody wrote.
- **The two policy installers cannot overwrite each other.** A reload
  and an emergency close arrive as separate processes into the same
  container, so the install is serialized by a lock the kernel holds and
  publishes through a private temporary file: neither a lost close nor a
  spliced document the relay would refuse to read.
- The relay's CONNECT port set, header handling, parser strictness and
  every concurrency and size bound are explicit, tested and fuzzed.
- **A tunnel ends when both of its directions do.** A client that shuts
  down its write side once its request is out has half-closed, not hung
  up: the relay forwards that half-close and keeps carrying the reply,
  so the response arrives whole. And the idle bound belongs to the
  tunnel rather than to each direction, so a large upload or a slow
  download — silent one way for its whole duration — is not cut off
  while it is streaming.
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
  whole range as a side effect; a policy carrying one is refused. The
  rule belongs to the policy rather than to the configuration file, so
  the live reload channel is held to it too.
  An entry naming what no connection reaches — loopback, multicast,
  broadcast, the unspecified address — is refused for the opposite
  reason: it cannot take effect, and accepting it would leave an allow
  rule in the rendered firewall while the gateway refused every request
  through it. Link-local is not in that group: it stays denied by
  default, because a cloud instance keeps its own credentials there, and
  an allowance naming one address in it now reaches that address rather
  than being accepted and quietly ignored. One address is the whole of
  what it can reach: an entry covering more of the range is refused,
  which the rule above does not do for it, since the baseline withholds
  link-local as a single range and an entry of exactly that is broader
  than nothing.
  Both lists are also held to being written as IPv4. An address in the
  v4-in-v6 form renders into the ruleset and matches nothing at decision
  time, which is the same split arrived at through notation.
  Public prefixes are accepted because the profile already permits the
  public internet — which is how a capsule reaches a service on the
  host's own public address, an address the runtime deny set withholds
  from facts static validation cannot see.
- **A capsule runs under the deny set in force, not the one its launch was
  handed.** A policy change reaches the gateways that exist when it is
  installed, and a gateway still being created is not one of them — so the
  launch that creates one proves it carries the current set before its job
  is authorized to start. Without that, a capsule launched while a network
  appeared kept the older, more permissive set for the whole life of its
  job, and no later pass revisited it: the record said the change was in
  force everywhere, so every pass after compared that set against itself
  and found nothing to do. In the ordinary case, where nothing moved while
  the capsule was being built, the check reaches no daemon at all. A
  restriction that could neither be installed into a gateway nor close it
  no longer enters the record either, so the next pass attempts it again
  rather than never trying it again.
- **A volume that already exists under a capsule's name is refused, not
  adopted.** Ownership is proven by labels and never by name, and every
  create behind the engine port reports a taken name so the caller can
  prove ownership before adopting anything. The daemon's volume create is
  the exception: it answers a taken name with the volume that is already
  there, keeping its original labels, and reports no error. So a volume
  another owner held could be confirmed as a lease's own and mounted as
  the data root of a privileged daemon. The adapter now inspects first and
  reports the collision, which is what sends the launcher to the ownership
  proof it already had.
- **A capsule that exits before it is ready says so, and says why.** A
  supervisor writes its terminal state and then exits, so that state is
  readable for an instant and a poll can miss it — and an exec into a
  container that is gone always fails, which the wait read as "not yet".
  A capsule that aborted was therefore polled for the full readiness
  deadline and reported as a timeout naming nothing actionable. The wait
  now asks the daemon whether the container is still there, and reports
  the exit code it left behind.

### State and operations

- **The pre-migration backup is proven to be one.** It is the only rollback
  that exists — there are no down migrations, deliberately — and what was
  proven about it was that a file appeared: an empty file passes that, and
  so does a copy taken after the upgrade. The suite now opens the copy and
  requires the pre-upgrade schema version, the rows that were live, and
  nothing the upgrade added. The schema identity check also covers the
  prefix already applied while migrations are pending, and each migration
  commits the fingerprint of the set up to itself — so an edited migration
  below a pending one is refused by name instead of failing later on a raw
  error, and an upgrade interrupted between two migrations resumes instead
  of refusing the database it is resuming. The runbook says what nothing
  prunes: the copies accumulate until removed by hand.
- **Capacity a lease consumed comes back.** An admission credit is returned
  by one read that says the lease reached released, and that read used to be
  abandoned on its first failure — on a store whose single connection is
  shared with every transition, every intent write, the disk monitor and the
  reconciler, so losing it is a moment of contention rather than a condition.
  The binding served one fewer job for the life of the process, with one line
  in the log and nothing in `runpool status` saying why. It is retried now.
  The same capacity was held two other ways: a launch whose very first
  transition failed returned without unwinding, where every other failure in
  it walks the lease back, so a transient error there cost minutes of a
  tier's capacity until a periodic pass noticed; and an emergency that closed
  admission stopped the broker being offered capacity without stopping a pass
  already looping over ready work from taking it, because the pressure is read
  once before that loop.
- **A message is acknowledged on the strength of everything it carried being
  written down.** A lifecycle event that could not be recorded was logged and
  stepped over, and the message acknowledged anyway — and an acknowledged
  message is never sent again, while nothing re-derives a cancellation from
  the provider. A cancelled workload stayed ready to lease, and the next
  scheduling pass spent a whole capsule on work the provider had already
  closed. The message is now left for redelivery, where recording is
  idempotent and replays it whole.
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
- The schema ships as **one reviewed baseline**; a migration added to it is
  forward-only and immutable.
- **The attempt owns execution evidence and disposition**; a
  `capsule_lease` owns only the host resources it consumes, and carries
  no provider identifier. GitHub's scale sets, runner ids and workflow
  runs live in the adapter's own tables.
- **There are no down migrations.** Restoring the pre-migration backup
  is the rollback, because a down script claims every schema change is
  losslessly reversible and a dropped column is not.
- **The status document drops `desired_state`.** The column admitted
  `present` and `absent`, nothing ever wrote `absent`, and the only
  query that could had no caller — so it described a capability the
  product does not have. `bindings[].desired_state` is gone from the v1
  document; `api_version` is unchanged, because a field nothing could
  vary is not a contract a consumer can have depended on.
- **A schema is identified by its contents.** The applied migrations'
  fingerprint is recorded with the schema, in the same transaction, and
  checked on every open. A version counter cannot tell two schemas apart
  when a baseline is edited in place, which is exactly
  when a database written by an earlier build would otherwise be accepted
  and then fail on a missing table.
- **A diagnosis answers when the daemon does not.** `runpool doctor`
  runs to completion against an unreachable daemon and reports every
  check that could be made, which is the state an operator runs it in.
- **A report answers its own question.** `runpool status` reports a
  capsule image it cannot resolve as a finding beside the rest of the
  document, rather than refusing to answer — so one unset environment
  variable no longer takes the lease list and the daemon comparison down
  with it.
- **A binding is identified by what an operator configured.** Its
  durable key is built from the target, runner group and scale set name,
  never from a parsed form of the address, so a deployment that changed
  nothing keeps the row its whole history hangs off. Bindings the
  configuration no longer claims are forgotten on the next start, unless
  they still own a delivery — that trail is kept.
- **A scale set that merely shares a name is a stranger's, and stays one.**
  Runpool adopts an existing scale set only when it can show it created that
  one: either the id matches what it recorded, or an earlier pass wrote down
  the intention to create that exact name and did not live to record the id.
  The intention is now written only once the provider has said the name is
  free, so a refusal leaves nothing behind — before, the refused pass wrote
  the very record that tells a refusal from a crash, and the next poll adopted
  the stranger it had just declined, served its owner's jobs on this host and
  would later have deleted it. Two targets differing only in letter case are
  refused as the duplicate they are, for the same reason: GitHub logins are
  case-insensitive, so accepting both would put two bindings on one remote set.
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
  process while capsules are still running. The whole shutdown is sized
  to end inside the deployment's stop grace period, and the deployment is
  sized against that whole number rather than a subset of its terms: the
  serve loops are waited out, the drain is spent, then every message
  session closes concurrently under one shared budget, so the bound does
  not grow with the binding count and the sessions are closed rather than
  left for the next start to wait out. An operator running their own
  compose file rather than the one shipped here sizes the stop grace
  period against that whole budget and not against the drain window: a
  value between the two ends mid-shutdown, and the platform kills the
  process with its message sessions still open. The periodic reconciler's recovery
  ends with that shutdown instead of running a budget of its own, because
  its work is resumable — the next start finds each lease where the
  shutdown left it. A
  drain window that elapses leaves live capsules exactly as they are for
  the next start's adoption — a dying controller never dismantles a
  running job on its way down.

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
- **A workload the provider reassigns is served under its new
  delivery.** The redelivery replaces the predecessor's attempt when that
  attempt provably consumed nothing — including one held for review,
  which is an open state and would otherwise stall the binding's whole
  ordered queue — and refuses when a start was authorized, where the
  redelivery waits for the predecessor's own lifecycle instead. A
  replacement never reaches the attempts the same delivery just
  recorded.
- **An attempt replaced mid-preparation is not started.** The edge
  before the one effect that can begin execution is a compare-and-swap,
  so a launch whose attempt was resolved while its capsule was being
  prepared stops there, and the capsule is torn down without a job.
- **A cancellation is durable before its message is given up.**
  Everything a broker message carries is written down before the message
  is acknowledged, for the same reason the assignment is: an acknowledged
  message is never sent again and nothing re-derives a cancellation from
  the provider, so one lost in that window costs a whole capsule on work
  the provider already closed.
- **Every call into a container is bounded.** The daemon's connection
  for an exec is handed over once and stops consulting the caller's
  context, so a container that accepts a command and answers nothing
  held the call open indefinitely — including the gateway control calls
  a policy refresh makes while every launch waits for it.
  The connection now ends with the context, and each gateway call
  carries its own bound. Inside a capsule the readiness probe is bounded
  the same way, so a daemon that never answers cannot spend the whole
  readiness budget in one call.
- **Shutdown is never held up by a launch it cannot see.** A launch runs
  on a context that deliberately outlives the serve loop, so its cleanup
  still completes once a shutdown has begun; the pass that maintains the
  egress policy waits for the same thing that launch holds. Waiting for
  it can now be given up, because the wait for the serve loops has no
  bound of its own — the budget is a claim about what they cost, not a
  timer — and past the deployment's grace period the difference is a
  kill, which leaves every message session open for the next start to
  wait out as a conflict.
- **A lease that starts while the sweep is looking keeps its capsule.**
  The sweep enumerates the daemon before it reads which leases are live,
  because an object exists only after the lease that owns it committed.
  Read the other way round, a lease committing between the two reads owns
  a container the sweep cannot account for: it would force-remove a
  capsule whose job is running and delete the records that clean up after
  it. Cache lane collection already stated that order for the same
  reason.
- **A session that will not clear says so in the report, not only in the
  log.** The broker holds a crashed predecessor's session until it
  expires by inactivity, and waiting that out is the ordinary shape of a
  restart. Past the point one expires by, it is not: the reason recorded
  against the binding changes to say the session is not clearing on its
  own and that only work already queued is being served, so
  `runpool status` distinguishes waiting from stuck instead of showing
  the same line either way. The binding keeps waiting, because the work
  it can serve without a broker is work it would otherwise stop serving,
  and the other bindings are unaffected.
- **The provider's own answer outranks the capsule's.** Every other
  account of whether a job was handed over comes from inside the capsule
  — the state it reports, the code it exits with — and the capsule is
  the thing running the job, whose own daemon socket the job holds by
  design. When deregistering the runner is refused because the provider
  still considers it busy with the job, that is a party which did not
  run the job saying it was handed over, and the attempt settles as
  started rather than returning to the queue on the capsule's word.
  Only that one account is replaced: what the host daemon says about a
  container it never started is not the capsule's word, and an outcome
  nobody could establish is still held for a person.
- **One rule decides what becomes of an attempt.** The two paths that end
  a serving — the finalizing transaction and the sweep that finds a lease
  nobody is driving — reach the same decision through the same function,
  so a workload cannot return to the queue on one and settle as a job
  that ran on the other. An attempt somebody else already resolved is
  left alone: the lease still releases and its capacity comes back.
- **A capsule that never handed the job over returns it to the queue.**
  The supervisor exits with a status it reserves for stopping before the
  start was authorized — the ordinary end of an idle capsule when a
  controller shuts down — and both the path that awaited a capsule and
  the one that adopted it read that status the same way. It outranks what
  was recorded when the start was authorized: that record is written when
  a capsule reports itself up, while this one is the capsule stating that
  nothing ever came after it. A job read the other way around is settled
  as one that ran and is never served again.
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
- **Uninstall clears the whole machine, including what an instance
  recorded about reaching its provider** — a success and a failure write
  the same row, so this is every instance that ever ran. It runs once,
  after the containers and the scale sets are already gone, so a row left
  behind is not a cosmetic leftover: a child table its foreign key still
  points at fails the delete of its parent and aborts the rest, leaving a
  half-removed instance and a database no supported command will clear.
  A table added later that references something uninstall deletes has to
  be cleared before it, and the build refuses a release where one is not.
- **The durability configuration the release qualifies is the one the
  product opens with.** The suite and the store share a single
  connection string rather than each holding a copy, so a pragma changed
  in the product is a pragma the qualification sees.
- The lease machine, the cleanup saga and disposition-by-evidence live
  in **`internal/lease`**, with their own tests. They know nothing about
  providers, which is what keeps cleanup working when one is
  unreachable.

- **A tier that validates is one the split can hand a usable share.**
  Under the restricted profile a lease's envelope covers its capsule and
  its egress gateway together, and only memory required the remainder to
  be usable: `cpu: "0.500000001"` or `pids: 129` validated and handed the
  capsule one nano-CPU or a pid limit of two. All three axes now carry the
  floor, so a tier that reports itself valid is one a runner and a daemon
  can start under.
- **The operator surface answers with what it saw.** `runpool doctor`
  reports the container engine's own connection error rather than the
  check's fixed "mount the socket" remedy, so a socket that is mounted but
  unreadable no longer tells an operator to do what they already did. A
  `gc` preview refuses a schema the controller has not migrated with the
  sentence explaining that the safe path is closed while the irreversible
  one is open, and `gc --apply` exits non-zero when an eviction could not
  be recorded in the audit log — a hole no later pass rewrites. And
  `attempts resolve` without `--apply` reads the attempt it promises to
  act on, so an id that does not exist, or one already settled, is
  refused where it is read instead of previewing as a confident action.

- **The two vocabularies that record what was decided are constrained by
  their columns.** An attempt's resolution and its review reason are
  closed sets, and both travelled to the database as bare strings beside
  free-text prose an operator wrote — a transposition the compiler could
  not see, into columns that accepted anything. Both are named types now,
  refused by the table when they are not a member, and NULL until the
  event they name has happened, like the review timestamps beside them.

- **A cancellation the provider reports is translated where the provider
  is known.** The controller decided what to do about a cancelled
  workload by comparing GitHub's own word for it, in the core. A
  provider that respelled it would have stopped cancellations
  cancelling — silently, because a comparison that stops matching raises
  nothing — and Runpool would have burned a capsule on a job already
  called off. The word is now spelled once, in the adapter, and what
  crosses into the domain is the fact.

- **The trail says when a start was authorized.** Evidence records how far
  an attempt got and never when it got there, so an attempt held because a
  start was authorized and its runtime could not be observed showed a
  timeline that jumped from the lease to the hold — with the authorization
  that caused it, the at-most-once line a person resolving the hold is
  deciding about, durable nowhere but a log that rotates. Every advance is
  in `runpool attempts inspect` now, with a time and with who observed it.

- **A capsule image that cannot run is refused by name, in about a second.**
  A tier may name its own capsule image, and one whose entrypoint crashes
  writes no control protocol — ever. Waiting out the declaration deadline
  for it spent thirty seconds per attempt and then reported a read
  failure, which is not the incompatibility the controller holds on: the
  tier retried the same broken image until its retry budget ran out,
  under a reason that named none of it. It is now held on the first
  attempt, naming the exit code. And the inspection an ambiguous start is
  held on carries the reason it could not be read, where a transport
  failure previously left the operator an exit code and an empty string.

- **The threat model names the exit code as the same surface the control
  file is.** Only the status the supervisor reserves for stopping before
  the hand-over proves the runner never owned the job; every other status
  reads as execution. That is the right direction for at-most-once — and
  it means a supervisor the kernel kills, an out-of-memory PID 1 on a
  tight tier, settles an attempt as completed where the runner never
  picked the job up. Nothing runs twice; the work comes back only because
  the provider re-offers it, and that dependency is now written down
  rather than assumed.

- **A capsule cannot start under an egress policy that has been
  superseded.** A policy pass records its set only once every gateway
  that was relaying carries it, so between the moment it stops
  enumerating and the moment it records, the snapshot still holds the
  older set — and a gateway created after that enumeration was never
  reached by the pass either. The confirmation each launch performs
  compared against that snapshot without waiting for the pass, saw
  nothing to do, and installed nothing; every later pass then compared
  the new set against itself and returned before any fan-out. The
  capsule relayed under a superseded set for the whole of its job.
- **A job that provably never ran is requeued after a restart, not
  settled.** The supervisor writes `running` once the start hand-over
  returns, before the runner is forked, so a capsule that aborts in that
  stretch exits carrying the code reserved for "the runner never owned
  the job". The live wait and adoption both read that code and requeue;
  a restart destroyed the container unread and settled the attempt as
  started. Same facts, answered differently by whether the controller
  happened to be alive.

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
  Every v1 document carries **`served`** as its discriminator: `false`
  is the whole pre-serve form, `true` carries the full shape, and a
  consumer branches on the field rather than on which fields exist. The
  capsule image it reports per tier is resolved by the same rule and
  resolver `serve` launches with.
- **Every reporting surface answers its own question.** `attempts list
  --json` emits an array in every state of the world — an instance that
  has never run holds no attempts — and `attempts inspect` of an id
  that cannot exist fails naming the reason. `config effective` prints
  the job ceiling and retry budget that govern, materialized by the
  defaults rather than resolved out of sight at read time.
- The **liveness probe** verifies the state is a database this build can
  read and that the serve loop's disk verdict is recent, so a corrupt
  database or a wedged loop restarts the container instead of passing a
  file-exists check.
- **A binding reports what it reaches.** Each one carries when a provider
  call for it last succeeded and what it cannot do now, so an instance
  with no work to do is distinguishable from one reaching nothing —
  which every other field in the document reads identically. The record
  is durable, so the answer survives the controller that stopped being
  able to reach anything.
- **A target is what its URL names.** Any `https` host at repository,
  organization or enterprise scope; the browser's `…/orgs/<name>`
  address translates to the organization it names, a clone URL's `.git`
  is trimmed while canonicalizing, and the provider's own page
  addresses are refused with the reason. Where each target's credential
  travels is stated rather than assumed: the first serve logs the host
  per target — at `Warn` when GitHub does not operate it — and `runpool
  doctor` names the host in every credential verdict. An empty personal
  access token is refused when the client is built, symmetric with the
  incomplete-App refusal beside it.
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

- **What a release builds for and what it qualifies are two lists.**
  `build/platform.lock.json` records one entry per platform whose suites
  were run and whose host facts were reviewed, so qualifying a second one
  is an added entry rather than a change to the file that is itself the
  proof of the gate. A host with no entry fails as not qualified on that
  platform, naming the ones that are, rather than as an architecture
  mismatch. `build/images.lock.json` states what a release builds for,
  which today is more than what has been qualified — and the support
  matrix says which is which instead of letting one imply the other.
  Neither the architecture nor the distribution is fixed by the code that
  reads the record: what it requires is that the selection is stated, and
  that the platform is one a release can build for.
  The image lock has one reader, which the controller, the release gate and
  the contract suites all share, and its references and digests are read with
  the registry's own grammar rather than cut out of a string: an entry no
  registry could resolve fails before anything is pulled.

- **A release publishes every platform it declares.** Each is built on a runner
  of its own architecture, so nothing is emulated and each standalone binary is
  verified by running it where it will run. The images are published as an index
  assembled from those builds, and the assembly fails if the index serves fewer
  platforms than the run produced. The controller embeds one capsule reference,
  the index digest, which is correct for either architecture because the daemon
  resolves the matching child at pull time. Promotion copies that index inside
  the registry rather than pulling and retagging, which would publish one
  platform of what was qualified.

- A release qualifies the exact digest-qualified controller and capsule
  candidates it later promotes; no image is rebuilt after qualification.
- Platform qualification compares every locked host fact and fails when a
  value is missing, not only when a reported subset differs.
- The standalone binary and completions are built once before qualification,
  retained by checksum, and published without rebuilding. Releases also carry
  separate controller and capsule SBOMs plus signed provenance attestations.
- **The preflight proves the isolated bridge took effect, not that the daemon
  accepted the request.** A daemon that does not recognise the option takes the
  request and drops it in silence, so a network that creates says nothing; what
  says something is that the daemon assigned the bridge no host address, which
  is the address a capsule would otherwise route through. The refusal names the
  address it found. This is also the first thing that fails when the option is
  removed from the code: it was possible to delete it and pass every gate.
  Under `unsafe-open-egress` the probe does not run at all: that profile
  attaches no capsule to the isolated bridge, so the check reports the
  unconfined egress rather than refusing the host over a capability nothing
  would use.
- **Runpool deploys on any Docker host that runs Compose.** What a platform has
  to provide is five things — a container kept running from a digest, a
  persistent volume, two read-only mounts, the socket, and a redeploy that keeps
  the volume — and a checklist says how to tell whether one of them does. There
  is no route in to provide: the controller is headless and its health is a
  command run inside the container, not an endpoint. Named platforms are
  examples, not integrations.
- The status document reports `engine_error` and the preflight names a
  `container engine`, rather than naming the one engine there is. What either
  says about Docker it says in the message, where it is true.
- Public-repository gates include CodeQL, dependency review, Dependabot,
  vulnerability scanning, SHA-pinned actions, and least-privilege workflow
  permissions.
- Every path the documentation names is checked, whether it is written as a
  link or as a command someone will copy, so a rename cannot leave a reference
  that reads as fact.
- Each live suite has one harness, run by the pull-request gate, by the release
  gate and by the driver that ships it to a host over SSH. The two gates differ
  only in whether qualification mode is on and whether the output is kept as
  evidence. The Docker suite's pull fixture is cleared by that harness and the
  removal is proven, so the test that exists to pull it cannot quietly stop
  pulling anything.
- The release-qualification record is one typed document with two ends: the
  job that writes it and the job that reads it back share the type, so a field
  one of them renames is a build failure rather than a publication that
  verifies nothing. It is assembled from the reviewed reference the controller
  itself embeds, and refuses evidence that does not support the claim it would
  make — a platform nobody qualified, an entry still pending, an end-to-end run
  of other images.
- Every workflow states the Go toolchain it builds with, held equal to the
  `toolchain` directive `go.mod` names by the same check that holds the builder
  images to it, so the binary a release ships is compiled by the toolchain its
  gates ran. The `go` directive is separate and lower: it is the oldest Go that
  may build this module, which is what the dependency graph requires rather
  than what a maintainer happens to run.
  Every job carries a deadline, and every script in the tree is linted.
- The job that builds the candidate images runs under its own protected
  environment: it writes to the registry before anything has been qualified.
- The qualification policy is **frozen in `build/platform.lock.json`**. Docker
  Engine 29.7.2 on Debian 13 is selected for the first qualification, and the
  reference host's exact facts were captured and reviewed before any release
  candidate existed. Contract suites fail closed while a lock is pending, and
  compare the host against the manifest once it is frozen.
- Release qualification runs its live suites **on the reference host** and
  builds its record from the evidence its gates emit: those suites, the
  upstream provider contracts against real fixtures, and the artifact
  checksums.
- No contract may be skipped in release-qualification mode.
- The controller E2E drives three real assignments through the exact image
  candidates: restart recovery, cache reuse, generation isolation, restricted
  egress, inner Docker build and registry push, remote cleanup, and preservation
  of unrelated container, network and volume sentinels by exact id.
- **A gate that authorizes a release is one that can fail.** The
  qualification record names only the suites whose evidence the run holds:
  each is paired with the artifact that proves it ran, and a missing or
  empty one refuses the record rather than stamping a suite that may never
  have executed. Every platform fact the reference declares is proven
  compared against the host's, driven from the fact set itself, so a fact
  added without a comparison is a failure rather than a silent hole. And
  each release build matrix must name every platform the image lock
  declares, because a union across the workflow stayed whole while one
  matrix lost a leg and published an index without its child.
