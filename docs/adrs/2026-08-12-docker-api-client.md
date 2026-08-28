# Use the versioned Moby client modules

**Status:** implemented
**Implemented by:** [the engine port](2026-08-23-the-engine-port-has-a-name.md), which names the adapter
**Date:** 2026-08-12

## Context

Runpool controls Docker through a privileged, long-lived API boundary. A
client error can misapply resource limits, omit ownership labels, leak an
object, or report the wrong process outcome. The dependency therefore needs a
versioned library API, maintained types, and live contract coverage; successful
compilation alone does not prove that option fields map to the intended daemon
request.

## Decision

Use `github.com/moby/moby/client` with the matching
`github.com/moby/moby/api` module. Keep all daemon operations behind the
Moby adapter; no domain package imports Moby types.

The adapter owns API-version negotiation, error normalization, ownership
labels, and the translation between Runpool specifications and Moby option
structures. Its live contract suite runs against a real daemon and verifies
the behaviours that are not meaningful to mock:

- container lifecycle, exit status, and stdout/stderr demultiplexing;
- secret delivery and ownership-label round trips;
- cgroup v2 memory, CPU, swap, and PID limits;
- volume, network, exec, image-pull, and cleanup semantics; and
- idempotent removal and cancellation-safe cleanup.

The adapter and API module versions are pinned in `go.mod`. Dependabot proposes
updates, and `govulncheck`, static analysis, hermetic tests, and the live suite
must pass before an update is accepted.

## Consequences

- Domain code depends on Runpool interfaces rather than a Docker SDK.
- Moby upgrades are reviewed as boundary changes and require the live Docker
  contract suite.
- Release qualification records behaviour against the exact Engine API in
  `build/platform.lock.json`; module compatibility alone is not runtime
  evidence.
