# Architecture decision records

One record per decision that constrains what the code may do. A record
is written when the decision is made, and amended only by another
record — a superseded ADR keeps its text, because the measurement that
justified it usually still holds even when the remedy does not.

A status is one of four words, and the word alone answers whether the
code has the decision:

| Status | What it says |
| --- | --- |
| `accepted` | Decided, and not built yet |
| `implemented` | Decided, and the code has it |
| `amended` | Stands, and a named part of it was replaced |
| `superseded` | Replaced entirely; the record that replaced it is named |

What replaced a decision, wholly or in part, is a line of its own in the
record rather than prose inside the status — a status that narrates is a
status two readers summarise differently.

| Date | Decision | Status |
| --- | --- | --- |
| 2026-08-11 | [SQLite driver](2026-08-11-sqlite-driver.md) — modernc, pinned and tested on a Linux named volume | implemented |
| 2026-08-11 | [Session conflict](2026-08-11-session-conflict.md) — one session per scale set, and what a conflict means | amended |
| 2026-08-11 | [Shared network namespace](2026-08-11-shared-network-namespace.md) — the runner joins dind rather than attaching itself | amended |
| 2026-08-11 | [Advertised capacity](2026-08-11-advertised-capacity.md) — capacity is a total, not a delta | amended |
| 2026-08-11 | [Capacity floor](2026-08-11-capacity-floor.md) — every binding floored at one | superseded |
| 2026-08-11 | [Repository cache scope](2026-08-11-repository-cache-scope.md) — persistent lanes only where identity binds | implemented |
| 2026-08-11 | [Plain L3 routing is rejected](2026-08-11-network-sandbox-proxy.md) — why an in-bridge gateway cannot forward, and the one path the host's rule leaves open | implemented |
| 2026-08-12 | [Docker API client](2026-08-12-docker-api-client.md) — versioned Moby modules behind a live-tested adapter | implemented |
| 2026-08-13 | [Egress is a relay, not a route](2026-08-13-egress-relay.md) — what the host's own `--internal` rule forces, and what it gives | implemented |
| 2026-08-13 | [Admission credits](2026-08-13-admission-credits.md) — tier parallelism is shared credit with a rotating discovery credit | implemented |
| 2026-08-14 | [Scheduling and swap semantics](2026-08-14-scheduling-and-swap.md) — optional global parallelism and provider-neutral resource units | implemented |
| 2026-08-17 | [Capsule image substitution](2026-08-17-capsule-image-substitution.md) — a tier may name its capsule, and the capsule declares its protocol | implemented |
| 2026-08-17 | [GitHub App credentials](2026-08-17-github-app-credentials.md) — a deployment authenticates as an App, not only as a person | implemented |
| 2026-08-17 | [Job timeout](2026-08-17-job-timeout.md) — the lease ceiling is a backstop above the provider's own maximum | implemented |
| 2026-08-17 | [Multiplatform locks](2026-08-17-multiplatform-locks.md) — a lock records the platforms qualified, not the only one that works | implemented |
| 2026-08-17 | [Target hosts and scopes](2026-08-17-target-hosts-and-scopes.md) — any host the protocol serves, at any scope it defines | implemented |
| 2026-08-23 | [The engine port has a name](2026-08-23-the-engine-port-has-a-name.md) — the container engine is a port, and the Moby client is one adapter behind it | implemented |
| 2026-08-20 | [The session wait has no deadline](2026-08-20-the-session-wait-has-no-deadline.md) — why giving up costs more than waiting, and what the report says instead | implemented |
| 2026-08-23 | [The launch proves its own gateway](2026-08-23-the-launch-proves-its-own-gateway.md) — a capsule cannot start under a deny set that is not the one in force | implemented |
| 2026-08-27 | [A resolution reaches the writer](2026-08-27-the-resolution-reaches-the-writer.md) — the controller applies operator decisions so resolving one attempt does not stop every tenant's CI | implemented |
| 2026-08-27 | [An adopted capsule is read in its own dictionary](2026-08-27-an-adopted-capsule-is-read-in-its-own-dictionary.md) — a state word is trusted only under the protocol the capsule declares; its exit code is trusted from every version | implemented |
