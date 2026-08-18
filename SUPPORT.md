# Support

Runpool is pre-release and unreleased. There are no published binaries,
no version support window, and no service commitment. What follows is
where to ask, and what the project can currently answer for.

## Where to ask

| You want to | Use |
| --- | --- |
| Report a bug | A GitHub issue, with the output of `runpool doctor` and `runpool status` |
| Report a vulnerability | [SECURITY.md](SECURITY.md) — never a public issue |
| Ask whether something is supported | The [support matrix](docs/reference/support-matrix.md), then an issue |
| Propose a change | Read [CONTRIBUTING.md](CONTRIBUTING.md) first |

## Implemented scope

These are the boundaries intended for the first qualified release; they are
not a support commitment while the project remains unreleased.

| Area | State |
| --- | --- |
| CI provider | GitHub Actions, as the single adapter. The control plane is provider-neutral by construction, but no other adapter exists or is promised. |
| Host | Linux amd64, rootful Docker Engine 28.0 or newer, cgroup v2 with the memory and pids controllers. Docker Engine 29.7.2 on Debian 13 is selected for the first release qualification; the remaining host facts have not yet been frozen. See the exact support matrix. |
| Scope | Repository-scoped and organization-scoped scale sets. Persistent cache lanes are repository-scoped only. |
| Egress | The restricted profile (implemented and tested live, not yet release-qualified) denies direct egress and permits proxy HTTP or CONNECT to allowed addresses on ports 80/443. See [the runbook](docs/runbook.md). |
| State | SQLite in a Docker named volume, one controller per state directory, enforced by a lock. |

## What is not supported

- Public fork pull requests. The isolation boundary is a network policy
  boundary, not a hostile-compute sandbox.
- Treating `shared-daemon` as containment for hostile or public-fork code.
- Deployments where platform-wide volume pruning cannot be disabled.
- Any release. The project is blocked on the controller end-to-end gate,
  external security review, and exact-platform release qualification before it
  publishes one. Nothing here is a production commitment.
