# Scheduling uses explicit parallelism and provider-neutral resource units

**Status:** accepted
**Date:** 2026-08-14

## Context

A single Runpool instance can serve several targets through several resource
tiers. Tier limits alone let each tier fill independently, which is useful on a
dedicated host but can overstate the safe workload set on a shared Docker
daemon. A host operator also needs configuration terms that describe product
intent rather than Docker API representation.

Docker calls CPU quota `NanoCPUs` and expresses `MemorySwap` as the combined
memory-plus-swap limit. Those are adapter mechanics. Exposing them in the
configuration makes a common request such as “allow 1 GiB of swap” require
knowledge of Docker's arithmetic and creates ambiguity around zero and
unlimited values.

## Decision

The configuration has two explicit scheduling levels:

- `tiers[].parallelism` limits active leases for one resource tier;
- optional `scheduling.parallelism` limits active leases across every target
  and tier in the instance.

Omitting the global field keeps tiers independent. Setting it constrains both
provider capacity and local admission. Zero is invalid rather than overloaded
to mean unlimited. An unreleased lease consumes capacity from durable
reservation through cleanup or quarantine, and startup reconciliation adopts
existing leases before admitting new work.

The public resource envelope uses `cpu`, `memory`, `swap`, and `pids`. `memory`
is the RAM hard limit and `swap` is additional swap. The Docker adapter derives
its combined `MemorySwap` value as `memory + swap`; `swap: 0B` disables swap and
Runpool never requests an unlimited value.

Preflight uses the same scheduling contract. Without a global limit it budgets
all tiers at full parallelism. With a global limit N it conservatively sums the
N largest eligible envelopes independently for CPU, memory, and swap, then adds
the host reserve. Configured swap also requires observable host capacity and
Docker/cgroup enforcement.

## Consequences

- A shared 8 CPU, 16 GiB host can publish several selectable tiers while an
  instance limit of one guarantees that only one lease consumes resources at a
  time.
- Provider announcements cannot accept more aggregate work than local
  admission can serve under the configured global limit.
- `runpool status` reports the scheduling mode, configured and effective
  parallelism, active and available capacity, and per-tier accounting.
- Changing an omitted global limit to a number is an intentional policy change,
  not a backward-compatible default adjustment.
- Swap is an emergency buffer, not ordinary capacity. Production guidance
  recommends encrypted persistent swap where CI secrets may reach memory.
