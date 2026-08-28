# Persistent cache binds to repository-scoped scale sets only

**Status:** implemented
**Date:** 2026-08-11

## Context

Persistent cache lanes are Runpool's core performance feature, and the
demand messages a scale-set session delivers expose the repository's
identity before anything is provisioned. The temptation follows
immediately: react to a message from repository A by provisioning a
runner with A's cache lane mounted. On a repository-scoped scale set
that is safe by construction — every job belongs to the same repository.
On an organization-scoped scale set it would leak one repository's cache
state into another's build, because nothing ties the runner to the job
that motivated it.

## Decision

Persistent local cache lanes exist only for repository-scoped scale
sets. Organization- and enterprise-scoped targets never mount a lane in
V1 — configuration validation rejects `cache.enabled` on them — and
their workflows use remote caches such as GitHub-managed `actions/cache`
instead. An organization repository that needs a hot local cache gets a
repository-scoped scale set of its own; the scope, not a runtime
heuristic, is what makes the binding safe.

## Evidence

`test/contract/githubactions/assignment_test.go`, executed live against a
real organization with `actions/scaleset` v0.4.0, proves the crossover
symmetrically:

- repository B's job and then repository A's job were assigned to one
  organization scale set;
- a JIT runner nominally provisioned "for A" executed **B's** job;
- a second runner nominally "for B" executed **A's** job;
- both runs concluded `completed/success`.

JIT generation accepts only a runner name and work folder — there is no
job or request parameter that could bind the runner.

The experiment also pinned two broker behaviors the architecture must
respect:

- **Announcing capacity is admission.** While the session advertises
  free capacity, queued jobs arrive directly as `JobAssigned` (with
  `runnerRequestId=0`) — no `JobAvailable`, no `AcquireJobs`, no
  per-job local acceptance step. The capacity allocator's control over
  advertised capacity is therefore the only admission gate.
- **Lifecycle messages are hints.** `JobCompleted` messages failed to
  arrive for over six minutes after both jobs had succeeded. Completion
  truth is the runner exiting plus polled state; cleanup triggers on
  runner exit, hints, timeouts and reconciliation — never one signal.

## Consequences

- The scheduler may use demand-message repository identity for
  diagnostics and lane pre-selection only when the scale set is
  repository-scoped; the cache manager cannot infer organization
  affinity from messages at all.
- A future local organization cache would require a mechanism that
  selects the namespace only after the runner knows its assigned
  repository, without making other namespaces reachable; nothing in the
  current upstream contract provides that.
