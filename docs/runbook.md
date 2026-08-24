# Runbook

Operator procedures for a running Runpool instance. `runpool status` is
always the first command. Installation and platform integration are
covered by the [deployment guide](deployment.md).

## Running these commands

Every command here reads the state directory, and in the reference
deployment that directory is a named volume mounted only inside the
controller's container. A `runpool` binary on the host sees nothing: it
looks at an empty default path and reports an instance that has not run
yet. Run them in the container instead. The image has no shell, which is
why `exec` names the binary directly rather than a shell that would
interpret a command line:

```bash
docker compose exec controller runpool status
```

Outside Compose, `docker exec <container> runpool status` does the same.
Running the binary on the host works when it is pointed at the state:
`RUNPOOL_STATE_DIR` selects the directory, and the configuration
variables the [configuration reference](reference/configuration.md) lists
select the rest. A read-only command needs no configuration at all.

Commands that change something take `--apply` or `--confirm`, preview
without it, and take the singleton lock the controller holds — so they
report that the controller is running rather than acting behind it.

## Shared-daemon operations

`runpool status` prints the configured topology and the scheduling picture —
mode, effective parallelism, active and available capacity, and a line per
tier — and `status --json` exposes the same under `host_topology` and
`scheduling`. On a `shared-daemon` host:

- keep the configured CPU, memory, swap and free-disk reserve above the
  measured peak of the platform and colocated applications;
- set `scheduling.parallelism` when the host budget must hold across every
  target and tier, rather than letting each tier fill independently;
- do not run `docker volume prune`, `docker system prune --volumes`, or a
  platform “full prune”; use Runpool GC for cache lanes;
- inspect ephemeral resources through `io.runpool.instance`,
  `io.runpool.lease`, `io.runpool.attempt`, `io.runpool.target`,
  `io.runpool.tier`, and `io.runpool.role` labels;
- investigate a doctor topology warning as an acknowledgement of shared
  compromise scope, not as a failed admission check.

Runpool removal commands filter by instance and re-inspect lease ownership
before each destructive intent. They never prune the daemon. An externally
removed idle uplink is recreated and its policy rediscovered before the next
capsule launches; an externally removed idle cache volume is recreated cold,
so its warm contents are lost but ownership does not cross.

## Disk pressure

The controller measures once a minute: free space and inodes of the
Docker daemon's storage filesystem (probed from inside it, so the
answer is correct however the controller is deployed) and the total
size of this instance's cache-lane volumes. The verdict is persisted —
`runpool status` shows it, and a restart resumes under it.

| Level | Meaning | Automatic response | Operator action |
| --- | --- | --- | --- |
| `normal` | Floors and watermarks respected | — | None |
| `high` | Managed cache crossed `cache.global.highWatermarkPercent` of `maxManagedBytes` | GC evicts free lanes (TTL, then LRU) down to the low watermark | None required; investigate if it recurs every pass |
| `soft_emergency` | Filesystem free space under `softEmergencyFreeBytes` or under `host.reserve.freeDisk`, or free inodes under 100k | **Admission closes** (running jobs finish; ready work waits durably) and GC evicts every free lane | Find what is eating the disk — it is usually not the cache. `docker system df`, then `runpool gc` for the managed view |
| `hard_emergency` | Free space under `hardEmergencyFreeBytes`, or free inodes under 10k | **Fail closed**: admission stays closed, nothing is deleted automatically, an error-level log is emitted | Free space by hand. The state database and active leases are preserved; do not delete volumes without `runpool gc` or `runpool cleanup`, which prove ownership first |

Recovery is hysteretic: an emergency releases only after free space
clears the effective soft floor (`max(softEmergencyFreeBytes,
host.reserve.freeDisk)`) by one soft−hard band, so admission does not
flap at the boundary. `high` releases at the low watermark.

Every level change, every GC eviction and every retention pass that
forgot something is recorded in the audit log
(`audit_log` table; evictions carry reason, size, repository and
generation).

## Garbage collection

The serving controller collects automatically under pressure. Manually:

```bash
runpool gc
```

is always a dry run and can inspect a live controller (read-only).
`runpool gc --apply` takes the maintenance lock, so it runs only
against a stopped controller — a serving instance already collects for
itself. `--aggressive` plans every free lane, not just expired and
over-budget ones.

GC collects two things, reported separately: this instance's cache-lane
volumes, and the record of leases that finished longer ago than
`retention.leaseHistory`.

Of the volumes it touches only this instance's own lanes, and never a
leased one: eviction deletes the lane's row first (refusing leased
rows atomically), so a lane a job holds — or wins in a race with the
plan — is skipped, reported as `skipped`, and untouched. Orphan lane
volumes (a crash between row delete and volume removal) are found by
their ownership labels and removed. Failures are retried by the next
pass; they are reported. Inside the serving controller they are never fatal; `runpool gc --apply` exits non-zero when any eviction failed, so a scheduled maintenance job branches on a real result.

Of the books it forgets only the runtime record — the `capsule_leases`
row. What the work did is the attempt's evidence and is never pruned. A
lease is skipped, not forced, while the attempt it served is still
unresolved or while it still owns a resource intent: the first is how a
crashed-mid-release attempt stays findable, and the second is a real leak
that must stay visible. The serving controller does the same pass on its
reconcile interval, so a running instance keeps its own books bounded.

## Capsule egress

Under the restricted profile (`network.profile: public-internet-only`) a
capsule has **no route to anything**. Its bridge is internal in Engine
28's isolated gateway mode, so the host kernel drops every packet the
capsule addresses beyond that bridge — public destinations included.
Egress happens through the capsule's own gateway container, which
resolves names and opens connections on its behalf, refusing any
address in the deny set that no allowance names (private ranges,
link-local metadata, the host's own networks, every Docker subnet, the
uplink itself).

The capsule is created with `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`
pointing at that gateway, inherited by the runner, the job and the inner
Docker daemon. What this means in practice:

- HTTPS and HTTP work — checkouts, package installs, image pulls,
  API calls.
- Direct connections and protocols that ignore proxy environment variables
  do **not** work. Proxy-aware clients may use HTTP or CONNECT to allowed
  addresses on ports 80 and 443; CONNECT is an opaque tunnel rather than
  application inspection. `git+ssh`, other ports, and non-DNS UDP fail
  closed. A workflow that needs transparent egress must use HTTPS where
  possible. `unsafe-open-egress` is available only with
  `dedicated-daemon`; shared mode rejects it.
- Service containers are unaffected: they run inside the capsule on its
  own daemon's network.

A job that hits the policy sees `403` from the relay with the reason,
and the gateway logs the denied host and address. Losing the gateway
does not open egress — it removes it.

### What a policy change does, and what a failure costs

The deny set is rediscovered every 5 minutes and each change is
classified before it is installed:

- a **restriction** (a host interface or Docker network appeared) must
  land. A gateway that will not take it is closed and removed, because
  until the new set is in force that capsule can reach an address the
  policy now denies. Its job loses egress and fails; that is the
  intended cost.
- a **relaxation** (something went away) may fail harmlessly. The
  capsule keeps the stricter set it started with and its work
  continues.
- **discovery failing at all** closes every gateway. An undiscovered
  network is indistinguishable from one that was never there, so a
  policy that cannot be shown to be current is not treated as current.
- **a restriction that could neither be installed nor closed** stops the
  pass. The new set is not recorded, so the next pass attempts it again,
  and the pass reports the failure — which closes every gateway on the
  host to all egress, on the same rule as a failed discovery. The log
  line names the container. **The remedy is the daemon holding it, not
  the policy**: a container that refuses both an exec and a bounded
  removal is a wedged daemon, and launches and teardowns are failing on
  it too. Remove the container by hand, or restart the daemon; egress
  returns on the next pass.

A capsule created while a change is being installed is not left behind.
The gateway does not exist yet when the pass enumerates, so the launch
that creates it proves it carries the set in force before the job is
authorized to start. A gateway that cannot be reached at that point fails
its launch: the attempt returns to the queue and the lease's resources
are removed.

The handover is atomic on each half — one kernel restore, one file
rename — and the window between them is deliberately the stricter of
the two: the firewall is installed first, so a newly denied destination
is already blocked while the relay still applies the old set. It is not
a single transaction across both, and does not claim to be. Established
connections are unaffected by a reload; revoking them is what closing
the gateway does.

## Physical capacity

`runpool doctor` fails when the worst workload set allowed by global and tier
parallelism plus `host.reserve` exceeds the CPU, memory, or swap the host
reports. `runpool serve` refuses to start on a failing report. Raise host
capacity or lower `scheduling.parallelism`, `tiers[].parallelism`, tier
resources, or the reserve.

Configured workload swap additionally requires Docker swap-limit support. Use
encrypted host swap where CI secrets may be paged to disk, and investigate
sustained swap activity as resource pressure rather than treating it as normal
capacity.

## A binding that cannot open its session

GitHub's broker allows one active message session per scale set, and a
controller killed without a clean shutdown leaves its session marked
active until the broker expires it by inactivity. A successor sees `409
Conflict` on every attempt to open one. This is ordinary on a restart:
the binding logs that it is waiting, and the wait costs nothing, because
adopted capsules are recovered before session creation is reached.

Past five minutes it stops being ordinary. The log moves to error level
and the reason recorded against the binding changes to say the session is
not clearing on its own — which is what `runpool status` shows, so
waiting and stuck are distinguishable there rather than only in the log.

The binding keeps waiting, and keeps serving what it already holds: work
already delivered and queued is launched without the broker. What it
cannot do is take new work, because that arrives over the session.

Nothing times this out, and nothing restarts on it. Restarting does not
clear the broker's session — only inactivity on the provider's side
does — so a controller reporting this needs a decision rather than a
kick. Check whether another controller is running against the same scale
set; if none is, the session expires on the provider's schedule and the
binding recovers on its next attempt.

## Manual review

`runpool status` lists attempts held for a person. Inspect one with
`runpool attempts inspect <id>`, which reports the evidence the decision
turns on and the provider identifiers for checking the run externally:

```bash
runpool attempts inspect <id>
```

Then decide it. Exactly one of the two decisions, and `--apply` to make
it.

The applying form takes the singleton lock, so **the controller has to be
stopped** — and on a shared host that is every tenant's CI, not just the
job in question. Read the attempt first, decide, and stop the controller
for the resolution rather than the other way round. A dry run needs no
lock, so everything except the last step can be done while serving
continues.

```bash
runpool attempts resolve <id> --retry --reason "..." --actor "<name>" --apply
```

```bash
runpool attempts resolve <id> --settle-may-have-run --reason "..." --actor "<name>" --apply
```

`--retry` returns the attempt to the queue; use it when the evidence
shows the work never began. `--settle-may-have-run` closes it as having
possibly executed; use it when it may have, and a second run would repeat
whatever external effects it had. The evidence line and the provider's own
UI are what tell those apart — an attempt held as `start_outcome_unknown`
is exactly the case where this instance could not tell.

**`--apply` needs the controller stopped.** It takes the same singleton
lock `serve` holds, and reports that it could not. A dry run does not.

An attempt is held for one of three reasons:

| Reason | What it means |
| --- | --- |
| `start_outcome_unknown` | The controller authorized a start and never learned whether execution began. Retrying could run the work twice. |
| `retry_budget_exhausted` | The work provably never began, and the attempt has used every serving `scheduling.retryBudget` allows. Retrying is safe; the question is why it keeps failing. |
| `capsule_incompatible` | The image this tier launches does not speak the controller's control protocol. The work never began, so retrying is safe and pointless — the next attempt launches the same image. Fix `tiers[].capsuleImage`, or pair the controller with the capsule it ships, then `--retry`. |

An operator's decision is not overruled by the retry counter: resolving
with `--retry` serves the attempt again whatever the budget says.

## Cleanup and uninstall

`runpool cleanup [--apply]` removes owned resources no live lease needs
— it never touches cache lanes, which belong to the instance, not to a
lease. `runpool uninstall --confirm=<instance-id>` removes everything
this instance owns, cache lanes included; a wrong confirmation id is
refused. Add `--delete-scale-sets` to remove the adapter resources recorded for
the instance. A remote deletion failure stops the command before provider
metadata is purged, so retrying is safe.

**What uninstall does not take with it**, because none of it is a
resource this instance owns and each is a separate decision:

- the state volume — though not the books: uninstall purges the
  delivery, attempt and lease records, so what the volume still holds
  afterwards is the instance identity and the audit record of
  maintenance actions, not a copy of the work;
- the pre-migration copies beside it, `pre-migration-v<n>.db`;
- the images Runpool pulled, and the credential files a deployment
  mounted;
- the deployment itself — the Compose service, its unit, its platform
  entry;
- without `--delete-scale-sets`, the scale sets on the provider, along
  with any runner registration that outlived its capsule.

## Quarantined leases

A lease reaches `quarantined` when its cleanup could not finish: an
object the daemon would not remove, a daemon that stopped answering
mid-release. It is not terminal — `released` is the only terminal state —
so a quarantined lease still holds its tier credit, and a pool with
several of them advertises less capacity than the host has.

There is no command for this, on purpose: the lease is not stuck on a
decision, it is stuck on an object. Clear the obstruction and the
periodic reconciler converges the lease on its own, without a restart.

```bash
docker compose exec controller runpool status
```

The `discrepancies` the report lists are where the books and the daemon
disagree, and a quarantined lease's resources are named in its own entry.
Find why the daemon will not remove the object — a container still
running, a volume still mounted, a network with an endpoint attached —
and remove the reason. The next reconciliation pass retries the release
and the credit comes back.

A quarantined lease is never swept as an orphan and never pruned by
retention: both would take the resources of a job that may still be
unwinding.

## Lifecycle procedures

Every procedure here is executed, not just described:
`scripts/drills/lifecycle.sh <host>` runs them all against a real
daemon and asserts each outcome, and the integration workflow runs the
same drills on every change.

**Install.** Build or pull the controller image, create the state
volume, and run `runpool doctor`. On a healthy, unconfigured host the
host checks all pass and doctor still exits non-zero naming the missing
configuration — that is the expected shape, not a failure. `serve`
refuses to start without a credential.

**Backup.** With no controller running (the singleton lock guarantees a
running one would be holding the state), archive the state volume. The
name is the one Docker created, not the one the Compose file declares:
Compose prefixes it with the project, so the reference deployment's
`runpool-state` is `runpool_runpool-state` on the daemon. Confirm it
before archiving — `docker run` creates a volume that does not exist,
which produces a valid, non-empty and completely empty archive.

```bash
docker volume ls --filter name=runpool
```

```bash
docker run --rm -v runpool_runpool-state:/state:ro busybox tar -czf - -C /state . > runpool-state-backup.tgz
```

```bash
tar -tzf runpool-state-backup.tgz | grep runpool.db
```

The last command is the check that matters: an archive without the
database is the failure this procedure has, and it is silent until a
restore needs it.

Cache lanes are never backed up: they are warm build state, disposable
by design, and the next job recreates them cold.

**Restore.** Recreate the volume and unpack the archive into it. The
instance identity, the schema and the audit trail come back with it;
`runpool status` on the restored volume is the verification.

**Upgrade.** Stop the controller, back up (above), start the new
version: it migrates the schema forward on open and refuses to serve if
it cannot.

Runpool identifies a schema by its contents, not by how many migrations
produced it, so a database written from migrations this build does not
have is refused by name rather than failing later on a missing table.
Before the first release that includes the reviewed baseline itself,
which is still edited in place: a state directory written by an earlier
pre-release build is refused and has to be recreated. After the release
the baseline is immutable and this can only report a genuine mismatch. It also takes its own pre-migration copy in the state
directory, named `pre-migration-v<version>.db` and never overwritten —
a retried upgrade gets `pre-migration-v<version>.1.db` rather than
destroying the copy taken before the attempt that broke things.

**Rollback** is restoring a pre-upgrade backup and starting the previous
version. Runpool has no down migrations: a schema change is not assumed to
be losslessly reversible, and restore also covers an upgrade that failed
half-way.

Nothing prunes those copies. Each is a full copy of the state database,
they survive `runpool uninstall`, and the disk monitor never evicts
state -- so on a long-lived deployment they accumulate until removed by
hand. Keep the newest copy for each version you might roll back to and
remove the rest once an upgrade has proven itself.

A database written by a build this one does not know is **refused, not
repaired**: the error says how to export from the build that wrote it,
or how to start clean. Runpool does not guess at a schema it cannot
account for.

**Uninstall.** `runpool uninstall --confirm=<instance-id>
--delete-scale-sets` removes every owned container, network and volume (cache
lanes included) plus the recorded remote scale sets, then the operator removes
the state volume. The drill proves a wrong id is refused and nothing locally
owned survives a correct one; the release E2E additionally proves remote scale
set removal.
