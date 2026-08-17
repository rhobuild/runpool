# Admission is a pool of credits with a rotating discovery credit

**Status:** accepted
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

The discovery credit rotates. Each empty long-poll advances it to the
next binding that could use it, skipping any binding that already has
demand or a running capsule — those can already see and be seen.
Skipping them is what bounds the wait: every silent binding holds the
credit within one pass of the pool.

So the floor's guarantee survives in a weaker, affordable form. Sight is
no longer a permanent per-binding reservation; it is a bounded wait for
a shared credit, funded only out of genuinely idle capacity.

`Hold` withholds new credit from every binding without touching running
ones. The disk monitor sets it when an emergency closes admission:
capacity that will not be served must not be announced.

## Consequences

- `sum(advertised) <= tier parallelism` holds for any single state of a pool, and
  the tests assert it across the states a pool passes through and under
  concurrent traffic. The invariant is a statement about the pool, so it
  is read through `AdvertisedAll`; summing per-binding calls samples as
  many instants as there are bindings and proves nothing.
- More bindings than tier parallelism is a legal configuration. The validator
  accepts it and the doctor warns instead of failing, because the cost
  is real but bounded: a first job for a silent binding may wait one
  rotation to be noticed.
- A binding announcing zero is expected, not a fault. `PoolReport` gives
  the operator the whole tier's accounting — demand, reserved,
  advertised, and who holds discovery — and the controller logs it
  whenever a binding's announcement changes.
- Release qualification includes the live upstream behaviour: a silent binding must
  discover a queued job through the rotating credit against the real broker.
  Hermetic tests prove the local allocation contract; they do not replace that
  provider evidence.
