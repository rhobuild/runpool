# A crashed controller's broker session must be waited out

**Status:** accepted; its retry deadline superseded by
[the session wait has no deadline](2026-08-20-the-session-wait-has-no-deadline.md)
**Date:** 2026-08-11

## Context

A scale set has at most one active message session in GitHub's broker.
The local singleton lock (an flock in the state volume) guarantees one
controller process, so a live duplicate is impossible. A *crashed*
controller is different: SIGKILL runs no deferred `Close`, and the broker
keeps the dead session marked active until it expires by inactivity. A
successor that opens a session immediately gets `409 Conflict:
"scaleset … already has an active session"`.

This surfaced in the live crash-recovery exercise: the successor adopted
the running capsule, then failed session creation with 409 and exited,
stranding the capsule.

## Decision

Session creation retries on the 409 with a fixed backoff up to a
deadline, logging why it is waiting. The successor already holds the
local lock, so waiting is correct — there is no live peer to yield to,
only a dead session to outlast. Startup order makes the wait free:
`reconcile` runs *before* session creation and launches capsule-adoption
goroutines, so an adopted capsule is awaited and cleaned up while the
session is still blocked.

## Evidence

Live: after a SIGKILL mid-capsule and an immediate restart, the
successor logged the wait six times over roughly fifty seconds, then
opened the session with the assigned job still in its backlog. The
adopted capsule was released — every container, network and volume
removed — during the wait, and the job concluded successfully.

## Consequences

- The retry deadline bounds how long a fresh start tolerates a stuck
  session before giving up and letting the platform restart it.
- This is distinct from local controller contention: here the singleton
  lock is already held and only the remote session lingers. Standby and
  handover between two controllers are not implemented.
- Reconcile-before-session is now a load-bearing ordering, not an
  incidental one, and is documented as such.
