# Runner and dockerd share one network namespace

**Status:** amended
**Amended by:** the single-capsule container, which shares one namespace by being one process tree rather than two containers
**Date:** 2026-08-11

## Context

CI workloads expect a Docker-published port to be reachable through
`localhost` from the runner. Separate network namespaces break that contract:
the daemon publishes into one namespace while the runner resolves localhost
inside another.

## Decision

The runner, dockerd, and every process the job launches share the execution
capsule's network namespace. The current implementation places the supervisor,
runner, and dockerd in one outer container; containers launched by the inner
daemon are reached through its normal port publishing in that same namespace.

The runner uses `DOCKER_HOST=unix:///var/run/docker.sock`. Dockerd's data root
is a fresh volume for each workload, and the workspace is fresh. No image,
layer, workspace, or credential survives unless it belongs to an explicitly
mounted cache lane.

## Evidence

The live capsule contract verifies the Docker client reaches the inner daemon,
a published container is reachable on localhost, and the workspace is
writable by the runner. It also verifies that the complete capsule stays
inside its aggregate cgroup envelope.

## Consequences

- Runner and inner daemon versions are treated as one tested image set.
- The outer egress policy has one capsule namespace to confine.
- One outer cgroup accounts for the runner, daemon, and nested job workload.
