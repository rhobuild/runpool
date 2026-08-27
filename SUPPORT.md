# Support

Runpool is open-source software with no service commitment. A version is
supported once it has passed release qualification, and the
[support matrix](docs/reference/support-matrix.md) states what that covers.
What follows is where to ask, and what the project can answer for.

## Where to ask

| You want to | Use |
| --- | --- |
| Report a bug | A GitHub issue, with the output of `runpool doctor` and `runpool status` |
| Report a vulnerability | [SECURITY.md](SECURITY.md) — never a public issue |
| Ask whether something is supported | The [support matrix](docs/reference/support-matrix.md), then an issue |
| Propose a change | Read [CONTRIBUTING.md](CONTRIBUTING.md) first |

## Implemented scope

These are the boundaries a qualified release covers. Anything outside them is
not a support commitment.

| Area | State |
| --- | --- |
| CI provider | GitHub Actions, as the single adapter. The control plane is provider-neutral by construction, but no other adapter exists or is promised. |
| Host | Linux amd64, rootful Docker Engine 28.0 or newer, cgroup v2 with the memory and pids controllers. Docker Engine 29.7.2 on Debian 13 is selected for the first release qualification, and the reference host's facts are frozen. See the exact support matrix. |
| Scope | Repository-scoped and organization-scoped scale sets. Persistent cache lanes are repository-scoped only. |
| Egress | The restricted profile denies direct egress and permits proxy HTTP or CONNECT to allowed addresses on ports 80/443. See [the runbook](docs/runbook.md). |
| State | SQLite in a Docker named volume, one controller per state directory, enforced by a lock. |

## What is not supported

- Public fork pull requests. The isolation boundary is a network policy
  boundary, not a hostile-compute sandbox.
- Treating `shared-daemon` as containment for hostile or public-fork code.
- Deployments where platform-wide volume pruning cannot be disabled.
- Any version that has not passed release qualification on the exact platform
  frozen for it. Nothing here is a production commitment.
