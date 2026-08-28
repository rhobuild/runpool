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

## Watching an instance

There is no metrics endpoint and no exporter, by decision — see
[the record](adrs/2026-08-28-the-status-document-is-the-metrics-interface.md).
`runpool status --json` is the machine-readable account, and it carries
an `api_version` so a script that reads it is written once. Ten
conditions are the ones that need a person, and all ten are in that
document:

| Condition | Where it shows | Why it needs a person |
| --- | --- | --- |
| Disk pressure at either emergency level | `disk_pressure.level` | Admission is closed; hard means freeing space by hand |
| An attempt held for manual review | `manual_review` | The product holds undecidable work for a person rather than guessing; a person who is never told defeats that |
| A binding that stopped reaching its provider | `bindings[].last_error_at` at or after `last_contact_at` | The provider sends nothing and says nothing; CI queues invisibly |
| A queue that is not draining | `scheduling.queued` above zero while `available` is too | Work is waiting with credits free, which is not a busy instance |
| The books and the daemon disagreeing | `discrepancies` | Reconciliation found something it will not act on alone |
| An unreadable container engine | `engine_error` | The status is being reported without the daemon's half |
| A live lease that has stopped moving | `leases[].terminal`, `state`, `created_at` | Quarantine is one way a lease comes to rest; a finalization that fails after its resources are already gone is another. Both hold an admission credit, so a host wedged this way loses capacity silently |
| A disk measurement that has stopped arriving | `disk_pressure.measured_at` | The probe runs a container to measure, so it fails exactly when the disk is full — and a failed measurement writes nothing, leaving the last `level` reading "normal" indefinitely |
| A capsule image that cannot be resolved | `capsule_image_error` | Every tier that names no `capsuleImage` of its own would launch the build's default, and this says what that default is not |
| An egress sandbox that closed itself, or stopped checking | `egress_sandbox.error`, `last_pass_at` | A rediscovery it cannot complete closes every gateway on the host to all egress, which is right and is also every running job losing its network at once |

This prints one line per condition and nothing at all when there is
nothing to look at, which is what makes it a cron job: `cron` mails the
operator only when a command writes output.

```bash
docker compose exec -T controller runpool status --json | jq -r '
  if .served != true then
    "not serving: \(.detail // "no detail")"
  else
    [ (if ((.disk_pressure.level // "normal") | test("emergency")) then
         "disk pressure is \(.disk_pressure.level); admission is closed" else empty end),
      (if ((.manual_review // []) | length) > 0 then
         "\((.manual_review | length)) attempt(s) held for a person" else empty end),
      (.bindings[]? | select(.last_error_at and (.last_contact_at == null or .last_error_at >= .last_contact_at))
         | "binding \(.target_id) has not reached its provider since \(.last_error_at)"),
      (if ((.discrepancies // []) | length) > 0 then
         "\((.discrepancies | length)) discrepancy between the books and the daemon" else empty end),
      (if ((.engine_error // "") != "") then
         "the container engine is unreadable: \(.engine_error)" else empty end),
      (if ((.scheduling.queued // 0) > 0 and (.scheduling.available // 0) > 0) then
         "\(.scheduling.queued) queued with \(.scheduling.available) free: the queue is not draining"
       else empty end),
      (.leases[]? | select(.terminal == false and .state != "workload_running"
                           and (now - (.created_at | fromdateiso8601)) > 1800)
         | "lease \(.id) has been \(.state) for over 30m and still holds a credit"),
      (.leases[]? | select(.terminal == false
                           and (now - (.created_at | fromdateiso8601)) > 608400)
         | "lease \(.id) has been live for over a week in state \(.state)"),
      (if ((now - ((.disk_pressure.measured_at // "1970-01-01T00:00:00Z") | fromdateiso8601)) > 900) then
         "the disk was last measured at \(.disk_pressure.measured_at // "never"), so the level below is that old"
       else empty end),
      (if ((.capsule_image_error // "") != "") then
         "the default capsule image cannot be resolved: \(.capsule_image_error)"
       else empty end),
      (if ((.egress_sandbox.error // "") != "") then
         "every gateway is closed to all egress: \(.egress_sandbox.error)"
       else empty end),
      (if (.egress_sandbox != null
           and (now - (.egress_sandbox.last_pass_at | fromdateiso8601)) > 1800) then
         "the egress policy was last rechecked at \(.egress_sandbox.last_pass_at)"
       else empty end)
    ] | .[]
  end'
```

An instance that has not run yet reports that, rather than passing for
healthy — a monitoring script that treats a missing answer as a good one
is the failure this replaces.

Two things it deliberately does not report. A queue with no free credits
is a busy instance, not a stuck one. And a binding that failed and then
reconnected is a binding that recovered, which is why the comparison is
against `last_contact_at` rather than the presence of an error.

The lease checks earn their place by covering the first one's blind spot.
A live lease is not terminal, so it counts toward `active` and reduces
`available` — and a host wedged on them has `available` at zero, which is
exactly the condition under which "the queue is not draining" stays
quiet. Without these lines, the worse the wedge, the less the rest of the
list has to say about it.

**Both lease checks ask about shape rather than about a named state, and
that is deliberate.** A list of states to worry about keeps losing to a
state machine: quarantine is one way a lease comes to rest, a
finalization that fails after its resources are already gone is another,
and the next one will not be on any list written today. So the first
check is "not terminal, not running work, and older than thirty
minutes" — thirty because a capsule's whole preparation is bounded at
fifteen, so nothing healthy sits in any other state that long. The second
is "not terminal and older than 169 hours", which catches a lease that
outlived its ceiling in any state at all, `workload_running` included.

169 rather than something tighter, and the reason is the point: the
default ceiling is eight hours, but `jobTimeout` is per-tier and the
validator accepts up to `168h`. A bound derived from the default would
fire on every healthy job of any tier legally configured above it —
anchoring to the default instead of to the limit is precisely the mistake
the rest of this section exists to avoid.

So it anchors to the limit, plus an hour. The extra hour is not slop: a
lease is created before its job starts and outlives it while cleaning up,
so a job that runs its full 168-hour ceiling leaves a lease slightly
older than that, and a bound set to exactly the maximum would fire on it.
This is a backstop rather than a tight bound — the thirty-minute check
does the real work, and this one says only that something has outlived
any ceiling this configuration could have permitted.

The egress sandbox is watched on both counts for the same reason. Its
rediscovery runs every five minutes, and a pass it cannot complete
closes every gateway on this host to all egress — correct, because a
policy that cannot be shown to be current is worse than none, and also
every running job losing its network at once. So one check asks whether
the last pass failed, and the other asks whether passes are still
arriving at all: thirty minutes is six intervals, which no healthy
instance misses. `egress_sandbox` is `null` on an instance that
maintains no policy, and neither check fires there.

The disk check is that reasoning applied to a measurement rather than a
state. `disk_pressure.level` is rewritten only by a measurement that
succeeded, so a probe that keeps failing leaves the last reading in place
indefinitely — and the probe runs a container to do its measuring, which
is precisely what stops working when a disk is full. Watching the level
alone would report "normal" straight through the failure it exists to
catch; the timestamp is what says whether the level is an answer or a
souvenir.

Where the output goes is the host's to decide: mail from `cron`, a
systemd timer's failure state, a ping to whatever the machine already
alerts with. Runpool does not choose a monitoring stack, and does not
ship this as a script — a file under `deploy/` that nothing exercises is
a claim with nothing behind it.

Rates and history are not here and are not missing. How long jobs took,
how many failed, how long they queued — the provider holds all of it for
the exact runs this host served, in its own interface. What is durable
on this side is in the store: the attempt trail, the audit log, and
evidence, which is never pruned.

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

`--apply` reaches a controller that is serving: the decision travels to
the process that holds the lock, and that process applies it. Nothing has
to stop. When no controller is running, the same command writes directly
under the lock instead, so the two cases are one command and the operator
chooses neither.

```bash
runpool attempts resolve <id> --retry --reason "..." --actor "<name>" --apply
```

```bash
runpool attempts resolve <id> --settle-may-have-run \
  --reason "..." --actor "<name>" --apply
```

`--retry` returns the attempt to the queue; use it when the evidence
shows the work never began. `--settle-may-have-run` closes it as having
possibly executed; use it when it may have, and a second run would repeat
whatever external effects it had. The evidence line and the provider's own
UI are what tell those apart — an attempt held as `start_outcome_unknown`
is exactly the case where this instance could not tell.

**Every write to this state belongs to whoever holds the lock.** That is
what the controller does not give up, and why the decision goes to it
rather than around it. Two answers mean something specific:

- *"the controller is running but does not answer resolutions"* — a
  controller older than the maintenance socket. Upgrade and restart it,
  or stop it and resolve directly.
- *"the resolution was sent but its outcome is unknown"* — the decision
  travelled and the answer was lost. Read `runpool attempts inspect <id>`
  before deciding again: a resolution that landed shows its reviewer, and
  running it a second time is refused rather than repeated.

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

A quarantined lease lists what it still holds, each object's kind, name
and state, because those are the work. Do not look in `discrepancies`:
that list is for objects belonging to no live lease, and a quarantined
lease is live, so nothing of its will ever appear there. `--json` carries
the same objects under `leases[].resources[]` for a script.

Find why the daemon will not remove the object — a container still
running, a volume still mounted, a network with an endpoint attached —
and remove the reason. The next reconciliation pass retries the release
and the credit comes back. That pass runs about once a minute, and only
takes a lease nothing has touched for two minutes, so a quarantine that
clears is not instant: give it a few minutes before looking again.

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
docker run --rm -v runpool_runpool-state:/state:ro busybox \
  tar -czf - -C /state . > runpool-state-backup.tgz
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

**Upgrade.** The platform performs it: point the deployment at the new
digest and recreate the container. Nothing about the state moves by hand.

```bash
docker compose exec controller runpool version --json   # what is running now
docker compose stop controller
```

Back up before the schema can move (above), then set `RUNPOOL_IMAGE` to
the new release's digest — verify it first, `deployment.md` covers how —
and start it:

```bash
docker compose up -d controller
docker compose exec controller runpool version --json
docker compose exec controller runpool status
```

`version --json` names the capsule it is paired with, and `status` reports
the schema it opened. Those two answers are the upgrade: the controller
migrates the schema forward on open and refuses to serve if it cannot.

Two things the platform must not do. **Do not start the new controller
before the old one has stopped** — the singleton lock refuses the second,
and with `restart: unless-stopped` that is a crash loop until the first
releases. Rolling and zero-downtime strategies are off. And **a release
that moves the capsule control protocol needs every derived
`tiers[].capsuleImage` rebuilt from the new published capsule**, or the
tiers using them hold their attempts as `capsule_incompatible`.

Runpool identifies a schema by its contents, not by how many migrations
produced it, so a database written from migrations this build does not
have is refused by name rather than failing later on a missing table.
The baseline is immutable, so a refusal reports a genuine mismatch
rather than a baseline that moved underneath the database.

It also takes its own pre-migration copy in the state
directory, named `pre-migration-v<version>.db` and never overwritten —
a retried upgrade gets `pre-migration-v<version>.1.db` rather than
destroying the copy taken before the attempt that broke things.

**Rollback.** Say which case you are in first, because two of the three
need no restore.

*The release moved no schema.* Point `RUNPOOL_IMAGE` back at the previous
digest and recreate. Nothing else: both builds read that database.
`runpool status` before rolling back tells you — if its schema version is
the one the old build wrote, this is the case you are in.

*The schema moved.* The old build refuses the database by name rather
than failing later, so the state has to go back too. Either restore the
archive taken above, or use the copy the migration took:

```bash
docker compose stop controller
docker run --rm -v runpool_runpool-state:/state busybox sh -c \
  'cd /state && rm -f runpool.db runpool.db-wal runpool.db-shm \
   && mv pre-migration-v1.db runpool.db'
# set RUNPOOL_IMAGE back to the previous digest, then:
docker compose up -d controller
docker compose exec controller runpool status
```

All three files, not just the database. The write-ahead log and the
shared-memory file beside it belong to the schema that was running when
they were written, and a restored database opened next to them is read
through the wrong one.

*Either way, a restore forfeits what was accepted after the copy was
taken.* Leases created under the new version are absent from the
restored books, so their capsules are labelled and unowned, and the
previous controller's sweep removes them: those jobs die and the provider
sees failed runs. If a rollback must not lose work, stop admitting before
upgrading rather than after.

Runpool has no down migrations: a schema change is not assumed to be
losslessly reversible, and a restore also covers an upgrade that failed
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
