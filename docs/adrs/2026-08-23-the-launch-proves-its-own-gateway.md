# The launch proves its own gateway

**Status:** implemented
**Date:** 2026-08-23

## Context

A capsule's egress is confined by a gateway container that carries a deny
set. The controller rediscovers that set periodically and before every
launch, and installs a change by fanning out to the gateways it can
enumerate. Once the fan-out returns, it records the new set as the one in
force.

The record is what the next pass compares against. That comparison asks
"is any gateway stale?" by comparing the controller's record with the
controller's record — a comparison that cannot see a gateway. Its
correctness rests on a claim the bookkeeping cannot support: that the set
of gateways at the moment of recording is the set that was enumerated.

Launches create gateways concurrently, so the claim is false. A launch
takes its copy of the sandbox, releases the serialising slot, and then
spends anywhere up to the capsule preparation budget creating containers
— which includes pulling an image the daemon may not hold. A policy pass
running inside that window enumerates the gateways that exist, does not
find the one being created, records the new set as in force, and returns.
The gateway then starts carrying the older set.

Nothing revisits it. Every later pass computes the same new set, compares
it against the recorded new set, finds no change and performs no fan-out
at all. The capsule keeps the older, more permissive deny set for the
whole life of its job.

The same record hides a second case. When a restriction cannot be
installed into a gateway, that gateway is closed; when the close also
fails, the failure was logged and the new set was recorded anyway. The
next pass then compares the new set against itself and never attempts the
restriction again, while the gateway keeps relaying past a deny the
operator was promised.

## Decision

**The launch that creates a gateway proves that gateway carries the set
in force, at the one moment it is both enumerable and not yet serving a
job.** A confirmation step runs after the capsule's preparation completes
and before the attempt is authorized to start. It compares the sandbox
the launch was handed against the one in force; when they agree it costs
two comparisons and reaches no daemon, and when they differ it installs
the current set into the gateways of that lease and re-reads to confirm
it was not overtaken. A gateway that cannot be reached, or a lease that
owns no running gateway, fails the launch — and the launch's own recovery
removes every resource of the lease, so a stale gateway is torn down
rather than left denying.

**A restriction that could neither be installed nor closed does not enter
the books, and the pass reports the failure.** The caller answers it the
way it already answers a failed enumeration: a launch is refused, and a
rediscovery closes every gateway to all egress.

**A gateway's first policy is installed through the lock a reload takes.**
The install that boots a gateway is the one installer with no predecessor
to read, and every other build states that it needs one instead of
inferring it from a read that failed.

Two alternatives were measured and rejected.

**Holding the refresh slot across the launch.** This makes the recording
claim true by preventing gateway creation during a pass, but the slot is
a single one and the hold would span the entire capsule preparation
budget — image pulls, readiness waits and the credential exchange. At the
parallelism this design already reasons about, launches begin failing on
the wait rather than queueing behind it, because a launch waits on its
own preparation context. Worse, the periodic rediscovery acquires the
same slot from a ticker loop, and a ticker drops ticks for a slow
receiver: to stop a launch crossing a tightening, this stops tightenings
being discovered.

**Re-asserting the policy on every pass.** Removing the unchanged-set
early return makes every pass fan out to every gateway. Because a
rediscovery also runs before every launch, this moves a cost bounded
per-change to per-launch — one control command per gateway per launch,
inside the slot every other launch waits for. It also does not close the
hole: a gateway created after a pass enumerates still misses that pass
and is corrected only by the next one, which may be after the job has
finished. It converts a permanent divergence into an intermittent one at
the highest running cost of the three.

## Consequences

A capsule cannot start under a deny set that is not the one in force. The
check is free in the ordinary case, where nothing moved while the capsule
was being built.

A gateway that refuses the policy in force fails its launch rather than
serving. The attempt returns to the queue and the lease's resources are
removed, which is the same answer an unprovable sandbox already gets.

A restriction that could neither be installed into a gateway nor close it
now costs egress for every capsule on the host, on every pass, until the
gateway is gone. This is deliberate: a gateway relaying past a deny is a
stronger reason to close everything than not knowing what the deny set
should be, which already closes everything. Reaching it requires a daemon
that refuses both an exec and a bounded removal, on which launches and
teardowns are failing too. The log line names the container and says the
remedy is the daemon rather than the policy.

**What stays open.** A relaxation that did not reach a gateway leaves
that capsule confined more tightly than the policy for the life of its
job, and nothing retries it. That is deliberate — the capsule's work
continues, and the cost of being too strict is a failed job rather than a
crossed boundary — but it is a divergence between the record and a
running gateway, and it is not reported as one.
