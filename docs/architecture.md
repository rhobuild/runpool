# Architecture

This document describes the implemented system and its enforced package
boundaries. Architectural decisions and the measurements behind them are
retained separately as [ADRs](adrs/README.md).

## The shape

```text
cmd/runpool
    -> internal/command          the CLI tree (Cobra), argument and exit-code contracts
    -> internal/app              composition, process lifecycle, the controller loops
        -> internal/assignment   deliveries, attempts, lifecycle vocabulary
        -> internal/allocator    admission credits
        -> internal/capsule      prepare, start, inspect one execution capsule
        -> internal/lease        the lease machine, the cleanup saga, disposition
        -> internal/egress       pure egress policy: deny sets and decisions
        -> internal/netsandbox   the egress policy lifecycle: discovery, snapshot, fan-out
        -> internal/cache        cache lanes and their garbage collection
        -> internal/disk         the disk-pressure state machine
        -> internal/doctor       host preflight
        -> internal/credential   provider tokens, read and guarded
        -> internal/store        all durable state: schema, migrations, queries
        -> internal/config       configuration, defaults, validation

cmd/capsule-supervisor           pid 1 inside a capsule: boots the runner,
                                 and in the gateway container runs the relay
    -> internal/gateway          the egress relay's server, proxy, DNS and firewall
    -> internal/egress

internal/githubactions           the provider adapter
internal/engine                  the container engine Runpool asks for
internal/engine/docker           the Moby adapter
```

`internal/gateway` hangs off the second binary, not the controller: the relay
runs inside the gateway container, and `internal/app` does not import it. The
controller's part is computing the policy and handing it over.

The tree above is what hangs off the two binaries, and three things under
`internal/` hang off neither: `internal/command/gen` and
`internal/store/schema/gen`, which write generated files, and
`internal/qualification`, which assembles and checks the record a release is
authorized against. They live beside the packages whose types they drive and
run through `go run`. An architecture test keeps the last one out of the
product: a gate that links into what it gates is a gate measuring itself.

The arrow never points from lifecycle or state domains to a provider. An
architecture test enforces it: core packages may not depend on
`internal/githubactions` or on the provider SDK, directly or
transitively. `internal/app` is deliberately exempt because injecting the
adapter is its job -- which is why the egress policy lifecycle lives in
`internal/netsandbox` rather than beside it: a security decision belongs
under the rules, not in the one package that is outside them. The configuration schema names GitHub scale-set settings
because GitHub Actions is the only supported provider; it does not claim a
generic provider API that does not exist.

`internal/app` is the composition boundary. It owns process lifecycle and
provider-facing control loops; durable state transitions and resource
lifecycle rules remain in the packages it composes. The reconciler stays at
this boundary because adoption needs both provider bindings and neutral lease
state. The architecture test prevents that provider dependency from moving
into a core package.

## One job, one capsule, one budget

A capsule is a single container: a first-party supervisor as PID 1, the
GitHub Actions runner, and the job's own Docker daemon, with every
container the job launches inside it. One container means one aggregate
cgroup, so a job cannot escape its tier by spawning work in a sibling.

The long-lived provider credential remains in the controller and never enters a
capsule. A per-runner JIT bundle travels over `exec` stdin onto a 0600 tmpfs;
the supervisor redirects the configuration files named by that bundle onto the
same tmpfs. GitHub's runner interface requires the encoded bundle as
`--jitconfig`, so it is transiently visible in the runner process's argv and to
code with the same process-inspection authority. That exposure is explicit:
Runpool relies on the bundle being one-run and short-lived, and does not claim
to isolate it from the workload it authorizes. It never enters controller
state, Docker container configuration, labels, environment variables, or logs.

Under the restricted network profile the capsule has **no route out**:
its bridge is internal in Engine 28's isolated gateway mode, so the
host kernel drops anything it addresses beyond that bridge. That deny
lives in the host's namespace, where a privileged capsule cannot reach
it. Egress is a per-capsule gateway that resolves names and opens
connections on the capsule's behalf, checking every resolved address
against the policy — which is also why DNS rebinding changes nothing:
the address checked is the address dialed.

The gateway is inside the lease's budget, not beside it. The tier is
split — a fixed reserve for the gateway, the rest for the capsule —
and both run under one parent cgroup, so the sum is the tier by
construction.

## Identity, and why work is never lost

Two keys carry the whole delivery machine:

- a **delivery** is `(binding, source delivery key)` — the provider's
  own message identity;
- an **attempt** is `(delivery, source workload key)` — one execution
  of one workload.

Provider identifiers that are *observed metadata* rather than identity
— runner request ids, workflow run ids — live in 1:1 adapter tables and
are never keys. A partial unique index allows one open attempt per
workload, which is what makes a redelivery idempotent. A requeued
assignment carries the same workload key, so the index does not tell the
two apart: it is what forces the requeue to supersede the open attempt
instead of opening a second one beside it.

Nothing is acknowledged before it is durable. Execution evidence is
monotonic and never guesses: `not_started`, `runtime_prepared`,
`execution_start_authorized`, `running_observed`, `exit_observed`.
Where evidence cannot decide, the attempt goes to manual review with a
reason, rather than being settled by inference.

**The attempt owns that evidence, and its disposition.** A
`capsule_lease` is the lifecycle of the host resources an attempt
consumes — reserved, provisioning, runtime registered, workload
running, draining, cleaning, released — and nothing more. It carries a
binding, the attempt it serves and the runtime's registered name; it
carries no provider identifier at all. Keeping the two apart is what
stops a cleanup state being read as an execution outcome, which is how
a job that never ran gets settled as if it had.

It does carry one measurement: what its own serving established about
whether the workload began. That is not lease state and is never read as
any — the disposition still rules on the attempt, from evidence and
from a proof. It is kept because cleanup can fail and be retried, and
the pass that measured the proof is then not the pass that disposes of
the attempt; a retry arrives having measured nothing, and would settle a
job that never ran.

An attempt whose work provably never began returns to the servable queue
and is served again, so it holds one lease per serving: at most one live
at a time, and the released ones are the record of what it cost. The
retry budget is what ends that — a failure that repeats is not one more
retries fix, so the attempt goes to review rather than being served
without limit.

The schema is one reviewed baseline, not a development history:
`000001_initial` is the whole of it, and a migration added to it is
forward-only and immutable. There are no
down scripts — restoring the backup taken before a migration is the
rollback, because a down script claims every schema change is
losslessly reversible and a dropped column is not.

## Everything created is recorded before it exists

Every external object is created under a durable **resource intent**:
planned, then creating, then confirmed with its id. A crash anywhere
leaves an intent whose deterministic name either finds the object or
proves its absence. Ownership is proven by labels, never by name — a
name collision is refused rather than adopted. One create needs the adapter
to make that true: the daemon answers a taken volume name with the volume
that is already there and no error, so the adapter inspects first and
reports the collision the rest of the port is built on. Destructive recovery repeats
that proof immediately before removal, using the expected instance and lease;
a stale intent cannot delete a foreign object that later reused its name.

A lease's admission credit is released only after the transaction that verifies
zero remaining intents commits.

## Capacity is credit

A tier holds as many credits as its configured parallelism. Running capsules hold theirs;
free credits follow demand, max-min fair; one still-unclaimed credit is
the tier's rotating discovery credit, which is what keeps a binding
with no demand signal from going blind to its own queue. The sum a tier
advertises never exceeds it.

When `scheduling.parallelism` is configured, the same accounting also has an
instance-wide ceiling across every tier and target. Provider announcements and
local admission consume the same credits, and startup adoption restores the
count before new work can enter. With no global limit, tiers remain independent.

## The host is a budget too

A monitor measures the daemon's filesystem from inside it and the
daemon-accounted size of every cache lane, and a pure state machine
decides the level: normal, high, soft emergency, hard emergency.
Recovery is hysteretic. High collects garbage; soft closes admission
and collects aggressively; hard fails closed and deletes nothing.

The host topology is part of configuration, not an installation assumption.
`shared-daemon` requires a positive reserve for colocated platform and
application services, restricted egress, and ownership-safe cleanup;
`dedicated-daemon` gives Runpool exclusive operational ownership. The mode
changes coexistence policy, not the underlying compromise boundary: either
controller holds host-root Docker API authority.

The restricted sandbox state is synchronized and copied per launch. Before
admitting a capsule, the controller re-proves the instance uplink and rebuilds
the discovered deny set. This observes networks created by other stacks and
recreates an idle uplink removed by external cleanup without sharing mutable
policy slices between concurrent capsules.

The copy is not the guarantee. A policy pass fans a change out to the gateways
it can enumerate, and a gateway still being created is not among them — so the
launch that created one proves it carries the set in force before its capsule
is authorized to start. That check is where a capsule launched across a
tightening is caught; without it the copy it was handed would confine it for
the whole life of its job, and no later pass would revisit it.

## Further reading

- [Threat model](security/threat-model.md) — what is defended and what is accepted
- [ADRs](adrs/README.md) — each irreversible choice with its evidence
- [Runbook](runbook.md) — every operational procedure
- [Release readiness](release-readiness.md) — what remains before a release
