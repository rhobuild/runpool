# Contributing to Runpool

Runpool is pre-release infrastructure software with a host-level trust
boundary. Contributions are welcome, but changes must be reviewable,
reproducible, and supported by evidence proportionate to their risk.

## Development prerequisites

- Go 1.26.6, as pinned by `go.mod`;
- Git;
- Linux with rootful Docker Engine for live container contracts;
- `shellcheck` for shell changes.

The hermetic suite does not require Docker or network access:

```bash
gofmt -w .
go vet ./...
go tool staticcheck ./...
go test -race -shuffle=on -count=1 ./...
go test -covermode=atomic -coverprofile=coverage.out -count=1 ./...
scripts/verify/coverage.sh coverage.out
go tool sqlc diff
go tool sqlc vet
go tool govulncheck ./...
```

Use the scripts under `scripts/contracts/` for live Docker, SQLite and
capsule contracts; the provider contract runs from its own CI workflow
against real credentials. Never use production credentials or targets for
test fixtures.

### Where coverage lives

The hermetic percentage is not the whole picture, and chasing it is how a
suite ends up asserting a mock instead of the system. Two rules decide
where a behaviour is proved.

**Logic is covered hermetically, and the bar is high.** Anything that is a
function of its inputs — admission arithmetic, the pressure state machine,
the egress decision, configuration validation, the lease machine, the
cache planner — is tested in its own package with no daemon. These are the
packages where a gap is a real gap:

| Package | Expected |
| --- | --- |
| `internal/assignment`, `internal/allocator`, `internal/disk` | ~100% |
| `internal/config`, `internal/egress`, `internal/credential` | ~95% |
| `internal/cache`, `internal/store`, `internal/lease` | ~80%, limited by SQL paths a fault has to be injected to reach |

Those are review expectations, not a gate: `scripts/verify/coverage.sh`
enforces one repo-wide floor (`RUNPOOL_COVERAGE_MIN`, 35% by default). A
per-package gate would fail on the mechanics packages below, which are
deliberately thin here and proved elsewhere.

**Mechanics are covered live.** `internal/platform/docker`,
`internal/platform/githubactions`, the daemon-facing half of
`internal/capsule` and the socket-facing half of `internal/gateway` are
thin translations of an external API. A unit test there asserts the mock,
so they are proved by the suites that run against a real daemon and a real
provider instead:

| Code | Proved by |
| --- | --- |
| Docker adapter, capsule lifecycle, cgroup and envelope enforcement | `scripts/contracts/docker.sh`, `scripts/contracts/capsule.sh` |
| Egress relay, DNS, firewall, bypass attempts | `test/contract/capsule` (`bypass_test.go`, `budget_test.go`) |
| Installing a policy into the kernel: `gateway.installPolicy`, `ApplyFirewall`, `ClassifyLegs` | `test/contract/capsule/bypass_test.go` — these shell out to `iptables-restore` and read the container's own interfaces, so faking them would be simulating the platform rather than isolating a dependency. What is decidable without one — `RenderIPTables`, `Policy.Validate`, `ClassifyPolicy` — is tested hermetically. |
| GitHub Actions adapter, scale sets, sessions, acquisition | `scripts/contracts/` provider suites under `test/contract/githubactions` |
| SQLite durability and crash recovery | `scripts/contracts/sqlite.sh` |
| Whole-controller recovery, cache reuse, restart | `test/e2e/controller` |

When a bug is found in that second group, the fix belongs in a pure
function the first group can reach. The capsule's execution observation is
the worked example: the decision the daemon's own facts support is
`classifySupervisorState`/`classifyContainerState`, tested hermetically,
while only the exec around it needs a container.

## Change design

- Keep provider vocabulary inside its adapter. Core packages use neutral,
  opaque identities; the architecture tests enforce dependency direction.
- Keep responsibilities cohesive. Split a package or component when its
  invariants, dependencies, or lifecycle can be tested independently.
- Comments explain exported contracts, invariants, or non-obvious reasons.
  They do not narrate the next statement or refer to temporary review phases.
- Errors include the failed operation and enough observed context to act.
- Destructive operations preview by default and prove ownership before
  changing Docker or durable state.
- Schema changes update the baseline migration, schema snapshot, sqlc
  queries, generated code, and tests together. After the first release,
  migrations become immutable and forward-only.

Dependencies are evaluated on maintenance, security history, release cadence,
API stability, transitive cost, and the amount of bespoke code they replace.
Standard-library code is preferred when it is clearer, but avoiding a mature
dependency is not a goal by itself. Go tooling belongs in `go.mod` and runs
through `go tool`; the project does not add a second language toolchain for
repository linting.

## Pull requests and commits

Open a focused pull request using the repository template. Describe the
observable change, what the investigation found, the evidence you observed, and
what would notice the behaviour regressing. Bug fixes include a regression test
that fails without the fix.

Paste evidence only where continuous integration cannot reach. A committed test
is the better record everywhere else, and the provider contracts are the one
surface that never runs on a pull request.

Use imperative, scoped commit subjects where helpful, for example
`fix(store): retain ambiguous attempts for review`. Keep generated outputs in
the same commit as their source.

Pull requests merge by merge commit, so your commits reach `main` unchanged.
The message you write is what someone reads while bisecting, long after the
review is gone; the pull request body is a review document and is not kept.

One commit per logical change. Squash fixup-only commits before merge, and do
not hide a meaningful design change inside a general cleanup.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Report vulnerabilities privately according to [SECURITY.md](SECURITY.md).
