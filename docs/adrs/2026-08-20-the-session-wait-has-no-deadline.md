# The session wait has no deadline; what changes is what it reports

**Status:** accepted, supersedes one consequence of
[session conflict](2026-08-11-session-conflict.md)

**Date:** 2026-08-20

## Context

[Session conflict](2026-08-11-session-conflict.md) decided that session
creation retries a broker 409 with a fixed backoff, and listed among its
consequences:

> The retry deadline bounds how long a fresh start tolerates a stuck
> session before giving up and letting the platform restart it.

The retry and the backoff were built. The deadline was not, and building
it showed why the consequence does not hold for this controller.

Two things were measured against the code as it stands.

**A binding cannot end the process by returning.** The serve loop waits
on every loop it started, and three of them — the periodic reconciler,
the disk monitor and the sandbox watch — return only when the serve
context is done. A binding that gave up therefore left `loops.Wait()`
blocked, so the process stayed up serving nothing, which is the state a
deadline exists to end. Worse, the error it left behind surfaced only
after a signal, so an operator's clean shutdown of that controller
exited non-zero.

**A binding whose session is stuck is not serving nothing.** The loop
drains ready attempts before it requires a session, deliberately — its
own comment says local work drains whether or not the broker is
reachable. Work already delivered and queued is served with no broker
involved at all. Returning from the loop discards that: attempts sit
ready with nobody to launch them until the process restarts.

And a restart does not clear the broker's session. Nothing on this side
can; the session expires by inactivity on the provider's, on the
provider's schedule.

## Decision

There is no deadline. A binding waits a conflict out for as long as it
lasts, because waiting costs nothing it was otherwise doing and stopping
costs the work it was.

What the five-minute grace marks is a change in what is reported, not in
what is done. Past it the reason recorded against the binding says the
session is not clearing on its own and that only work already queued is
being served — a different string from the 409 recorded while the wait
is still ordinary, so `runpool status` distinguishes the two states
rather than showing the same line for ten minutes and then for ever.

A run of conflicts is a run of conflicts: a failure to open a session
that is not a conflict ends it, so time a binding spent unable to reach
the broker at all is not counted as time it spent waiting one out.

## Consequences

- A permanently stuck session is an operator's decision, not the
  controller's. The report names the state and the runbook says what to
  check; nothing exits on its own.
- The five-minute grace now has two effects rather than one: the log
  level it always changed, and the reported reason.
- A controller with one stuck binding keeps serving its other bindings,
  and the stuck one keeps serving what it already holds.
