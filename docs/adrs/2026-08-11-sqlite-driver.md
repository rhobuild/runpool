# SQLite driver: modernc.org/sqlite

**Status:** implemented
**Date:** 2026-08-11

## Context

The state store is Runpool's only durable component: lease transitions,
ownership records and reconciliation intent live in one SQLite database on
a Docker named volume. The driver choice shapes the release pipeline —
Runpool produces a static Linux binary, so a CGo dependency would add a C
toolchain to the build and complicate future architecture support.

Candidates: `mattn/go-sqlite3` (CGo bindings over the canonical library,
battle-tested, but complicates static multi-architecture builds) and
`modernc.org/sqlite` (a CGo-free Go port). The CGo-free option is acceptable
only while it passes the durability contract on the same storage topology used
in production.

## Decision

Pin `modernc.org/sqlite` v1.56.0 (embedded SQLite 3.53.3) together with
its transitive `modernc.org/libc` v1.74.4; the pair is one critical
dependency and moves only together, re-running the durability contract on
every update.

Connection contract, applied through the DSN so every pooled connection
is configured identically:

- `journal_mode=WAL`, `synchronous=FULL` — a committed state transition
  survives abrupt termination;
- `foreign_keys=1`, enforced, not just enabled;
- `busy_timeout=10000` and `_txlock=immediate` — writers serialize
  without the deferred-upgrade deadlock;
- the state store itself is a single writer (`SetMaxOpenConns(1)`);
- online backup uses `VACUUM INTO` (consistent snapshot under live
  writers), never a file copy;
- migrations move DDL and `PRAGMA user_version` atomically in one
  forward transaction. Rollback restores the pre-migration backup.

## Evidence

`test/contract/sqlite`, executed locally by
`scripts/contracts/sqlite.sh` and in the integration workflow:

- race-detector pass on darwin/arm64 — the driver is pure Go, so the
  detector instruments it fully;
- static linux/amd64 binary (`CGO_ENABLED=0`) against a Docker named
  volume on ext4, Engine 28:
  - 12 SIGKILL rounds against a live writer, then 3 rounds killing the
    whole container: `integrity_check` ok every time, no sequence holes,
    the meta counter atomic with its entries, every log-confirmed commit
    present;
  - 3 concurrent writer processes with immediate transactions: 600/600
    committed exactly once;
  - standby-lock behavior: a second writer fails busy near its timeout —
    never hangs, never corrupts — and proceeds after handover;
  - `VACUUM INTO` under a live writer yields a consistent, restorable
    snapshot;
  - capped-filesystem exhaustion: clean `SQLITE_FULL`, database intact;
    after freeing external space, writes resume — mirroring the disk
    monitor's emergency policy, which evicts cache, never state;
  - forward migration and backup-restore rollback.

## Consequences

- Release builds are static and require no C toolchain. Release qualification
  still runs the durability suite on the exact reference host.
- Escape valve: if the pinned driver regresses, `mattn/go-sqlite3` fits
  behind the same store interface — the contract suite is driver-agnostic
  apart from the import and DSN syntax.
- The suite stays gated (`RUNPOOL_SQLITE_CONTRACT_DIR`), so `go test
  ./...` remains fast and hermetic.
