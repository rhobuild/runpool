# Admission is a pool of credits with a rotating discovery credit

**Status:** implemented
**Date:** 2026-08-13
**Supersedes:** [the per-binding capacity floor](2026-08-11-capacity-floor.md)

## Context

The floor ADR established a measured fact and a temporary remedy. The
fact: a binding whose session announces `0` is not throttled, it is
blinded — GitHub delivers it no job and no statistics, so it can never
learn it has queued work and never raise itself. The remedy: floor every
live binding at one advertised capacity unit, permanently.

The remedy cost the invariant. With floors, `sum(advertised)` exceeds a
tier's parallelism whenever running capsules already fill it, and a tier with
more bindings than parallelism cannot be configured at all — the allocator,
the config validator and the doctor all had to refuse it. The overshoot
was absorbed as backlog because the physical gate (`TryReserve`) is
absolute, but "the promise is wrong and the gate catches it" is not a
capacity model.

## Decision

A tier holds exactly as many credits as its configured parallelism. They are distributed, in
this order, from one consistent read of the pool:

1. **Running capsules hold their credit.** A job in flight cannot be
   retracted, so advertised is never below active.
2. **Free credits follow demand,** max-min fair by water-filling, ties
   broken by registration order. Deterministic, so two callers reading
   the pool at the same state compute the same distribution.
3. **One credit, if still unclaimed, is the discovery credit.** It goes
   to a single binding with no demand signal and no running capsule.

The allocator publishes that distribution as an immutable `AllocationPlan`.
Allocation changes advance its generation and invalidate the cached plan;
remote poll acknowledgements do not rebuild a desired distribution that did
not change. Water-filling is performed in batches in O(B log P), where B is the
number of bindings and P is the configured parallelism, rather than scanning
every binding once per credit.

The discovery credit rotates. A successful empty long-poll advances it only
when that poll was made by the holder under the current discovery generation.
An empty poll from a binding at zero, or from another tier, carries no evidence
about the holder and cannot move it. Bindings with demand or a running capsule
are skipped because they can already see and be seen.

Provider polls are concurrent, so a desired snapshot is not by itself the
remote invariant. The allocator also accounts for the last capacity a session
confirmed and every poll in flight. An increase reserves its remote credit
before the request begins; a decrease releases the old value only after a
successful response. The next discovery holder therefore remains at zero until
the previous holder has confirmed its revoke. On restart, a tier remains at
zero until every predecessor session sharing its budget has been replaced or
closed; under an instance-wide limit that requirement spans all tiers.

So the floor's guarantee survives in a weaker, affordable form. Sight is
no longer a permanent per-binding reservation; it is a bounded wait for
a shared credit, funded only out of genuinely idle capacity.

`Hold` withholds new credit from every binding without touching running
ones. The disk monitor sets it when an emergency closes admission:
capacity that will not be served must not be announced.

## Consequences

- `sum(advertised) <= tier parallelism` holds for any single desired state,
  while the maximum capacity that concurrent polls may have established
  remotely is bounded separately. Tests cover pending increases, failed
  revokes, session replacement, cross-tier isolation and global limits.
- More bindings than tier parallelism is a legal configuration. The validator
  accepts it and the doctor warns instead of failing, because the cost
  is real but bounded: a first job for a silent binding may wait one
  rotation to be noticed.
- A binding announcing zero is expected, not a fault. `PoolReport` gives
  the operator the whole tier's accounting — demand, reserved,
  advertised, and who holds discovery — and the controller logs it
  whenever a binding's announcement changes.
- Release qualification must include the live upstream behaviour: multiple
  silent bindings must discover queued work through the rotating credit against
  the real broker. Hermetic tests prove the local protocol; they do not replace
  provider evidence.
- The supported-scale benchmark covers 10,000 bindings and 10,000 credits in
  independent-tier and global-limit modes. Rebuilds budget 20 ms, bounded
  candidate visits, and bounded allocations; cached reads allocate nothing.
