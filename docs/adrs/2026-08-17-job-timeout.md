# The lease ceiling is a backstop, not the job's timeout

**Status:** accepted
**Date:** 2026-08-17

`jobTimeout` is two hours, declared as a constant in `internal/app`, and
it bounds the context that wraps a whole lease — both a lease this
controller started and one it adopted at restart. Expiry cancels
whatever that lease is doing, which in practice is the wait on the
capsule, because every other step on the path is short.

GitHub's own maximum for a job is 360 minutes. A workflow that sets
nothing, or sets anything above two hours, is therefore killed by
Runpool before its own provider would end it, and no document mentions a
time limit at all.

## The job's own timeout is not in the protocol

The provider's job message carries the runner request id, the repository
and owner, the job id, the workflow ref, the display name, the workflow
run id, the event name, the requested labels and four timestamps. It
does not carry `timeout-minutes`. There is no field to read and no
second call that would return one: the timeout is enforced by the
provider against the job, and Runpool is told about the job, not about
its schedule.

So the ceiling cannot be the job's timeout. The question is what else it
should be.

## What the ceiling is for

The provider already ends a job at its own limit. When it does, the
runner exits, the supervisor observes the exit, and the lease resolves
through the normal path. The ceiling never fires in that case.

It fires when the runner does not exit: a wedged process, a daemon that
will not stop, a capsule that has lost the ability to report. Without it
a lease holds its credit and its privileged container indefinitely.

That makes it a backstop against a stuck capsule, and a backstop belongs
**above** the largest legitimate run, not below it. Two hours is below
the provider's own maximum, so today it truncates healthy jobs to
protect against unhealthy ones.

## Decision

**The ceiling is configurable per tier and defaults to eight hours.**

```yaml
tiers:
  - id: standard
    jobTimeout: 8h
```

The provider's own maximum for a job is 360 minutes, which is six hours
exactly — so six is not a margin, it is a tie, and the two timeouts would
race. The provider's timeout is the one that resolves a healthy lease,
and it still has to reach the runner, exit it and be observed here. Eight
hours buys that sequence. A tier that knows its work is short may lower
it and get its capacity back sooner.

**Expiry is named.** The lease resolves with a reason that says the
capsule exceeded the tier's ceiling, so the attempt's evidence
distinguishes a Runpool backstop from a provider timeout and from a
crash.

**The value is documented** as what it is: a limit on how long Runpool
waits for a capsule, not a limit on how long a job may run.

## Consequences

- A workflow within the provider's own limits is no longer ended by the
  control plane.
- A stuck capsule still cannot hold a credit forever, which is the
  property the constant existed to provide.
- The ceiling is per tier because the tier is where the workload's shape
  is already declared; an instance-wide value would force the shortest
  tier's tolerance onto the longest.
- An adopted lease inherits the ceiling of the tier it belongs to, so a
  restart does not extend or shorten a capsule's remaining tolerance
  beyond what its tier declares.
