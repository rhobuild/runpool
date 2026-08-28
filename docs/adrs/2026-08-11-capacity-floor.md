# A zero-capacity binding cannot discover queued demand

**Status:** superseded
**Superseded by:** [admission credits](2026-08-13-admission-credits.md)
**Date:** 2026-08-11

## Context

A controlled broker experiment showed that a session advertising zero
capacity received neither assigned jobs nor statistics for work queued while
it remained at zero. Raising capacity later in the same session did not make
that queued work visible. A binding with no current demand signal could
therefore become blind to its first queued job.

## Original decision

The initial remedy assigned every binding a permanent capacity floor of one
and enforced physical concurrency separately at capsule launch. This kept
bindings visible, but allowed the sum of advertised capacity to exceed the
tier's parallelism when the number of bindings was larger than available capacity.

## Superseding decision

[Admission credits](2026-08-13-admission-credits.md) retain the measured
requirement without permanent over-advertisement. One idle credit rotates
among bindings that need discovery, while running work and active demand hold
the remaining credits. Total advertised capacity never exceeds tier
parallelism.

## Consequences

- The zero-capacity behaviour remains a live upstream contract.
- Local admission remains a safety limit independent of provider state.
- A change in broker behaviour can be evaluated against the contract rather
  than inferred from documentation.
