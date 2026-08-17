# Advertised capacity is a total, and zero is forever

**Status:** accepted; allocation refined by [admission credits](2026-08-13-admission-credits.md)
**Date:** 2026-08-11

## Context

Each message-session poll announces a capacity to the broker. Two
interpretations exist: the instantaneous free gap (parallelism minus active),
or the design's `advertised = active + grant` — the total number of
jobs the scale set may hold at once.

The first live crash-recovery exercise settled it. The controller
announced the free gap; while an adopted capsule was running the gap
was zero, and a job queued in exactly that window. The broker excluded
it — and never reconsidered: twelve minutes of subsequent polls
announcing free capacity, plus two completely fresh sessions, and the
job stayed `queued` with an assigned backlog of zero. Only cancelling
and re-dispatching the run recovered it.

## Decision

- The announced capacity is the **total** the binding may hold — the
  credits allocated to it while the controller is willing to serve — never
  the instantaneous free gap. The broker keeps up to that many jobs
  assigned and replaces one as soon as a job finishes.
- The broker may therefore hand over a replacement before local cleanup
  returns admission capacity. Durable ready attempts bridge that interval;
  delivery and attempt identities make redelivery idempotent without relying
  on an in-memory queue.
- Announcing less than the intended total is reserved for shutting
  down; the assignment decision happens once, when the job queues, so
  an underannouncement is not a throttle — it is a trapdoor.

## Consequences

- The capacity allocator moves *totals* between bindings with
  `advertised_i = active_i + grant_i`; it never expresses "busy" by
  announcing zero, because zero excludes rather than defers.
- Recovery policy for a job that queued against a zero announcement is
  outside the session protocol entirely: it requires re-queueing the
  run. The operator documentation must say so.
- Statistics remain the demand truth; this decision is about what we
  tell the broker, not about what we believe from it.
