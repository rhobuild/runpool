# Continuous integration

What each gate proves, where it runs, and the names the workflows use.
[Repository settings](repository-settings.md) holds the half that lives in
GitHub rather than in this tree.

## The gates

Every gate is a job. A gate that needs a real Docker daemon says so, because
that is what decides whether it can run on a pull request at all.

| Workflow | Job | Proves | Runs on |
| --- | --- | --- | --- |
| `ci` | `dependency changes` | No dependency change brings a known-vulnerable or unreviewed module | Hosted, pull requests only |
| `ci` | `the Go tree, its tests and its generated files` | Formatting, vet, static analysis, the raced suite with its coverage floor, the generated persistence layer and CLI reference are current, the module is tidy, the release binaries cross-build, the configuration examples validate, and the locked image digests have not drifted | Hosted; the digest check needs the network |
| `ci` | `workflows, scripts and containers` | Workflows, shell scripts and Dockerfiles lint, and the reference deployment resolves | Hosted; needs a daemon for hadolint and `compose config` |
| `ci` | `known vulnerabilities` | No package in the build has a known vulnerability | Hosted |
| `ci` | `controller image` | The controller image builds, its runtime layer carries no shell, and it runs | Hosted; needs a daemon |
| `codeql` | `security analysis` | Static security analysis of the built tree | Hosted |
| `integration-docker` | `moby adapter, cache lanes and disk` | The Docker adapter, cache lanes and disk pressure against a real daemon | Hosted daemon; path-filtered, plus nightly |
| `integration-docker` | `outer capsule, JIT and the egress sandbox` | A real capsule, its credential channel and the egress sandbox, leaving no managed object behind | Hosted daemon |
| `integration-docker` | `lifecycle drills` | Install, backup, restore, upgrade and uninstall, end to end | Hosted daemon |
| `integration-docker` | `sqlite durability` | Durability on a named volume, including the disk-full case, across kills | Hosted daemon |
| `contracts-github-actions` | `runner scale set API` | The provider adapter against the real scale-set API | Protected `upstream-contracts`; weekly, and during qualification |
| `controller-e2e` | `real assignment, restart, cache and cleanup` | The exact controller and capsule candidates running real assignments | Self-hosted, protected `release-qualification` |
| `qualify-release` | `validate release inputs` | The ref is a protected tag, the images are digest-qualified, and the standalone candidate is the tag it claims | Hosted |
| `qualify-release` | `live contracts on the reference host` | Every live suite, without skips, on the reference host | Self-hosted, protected |
| `qualify-release` | `release-qualification record` | The evidence supports the claim the record makes | Hosted |
| `release` | `build immutable candidates` | The candidate images and standalone artifacts exist by digest and checksum | Hosted, protected `release-candidate` |
| `release` | `attest and publish qualified artifacts` | The record covers this build, the artifacts match their checksums, and the promoted digests are the qualified ones | Hosted, protected `release` |

`qualify-release` also calls `ci`, `contracts-github-actions` and
`controller-e2e` rather than restating them: two definitions of one gate drift,
and the one that decides a release must be the one every change already passed.

Checks that are not gates in a Go file live beside the code they check.
`test/consistency` holds the ones that tie together values in different
ecosystems — a documented path and the file it names, a workflow's toolchain and
`go.mod` — and `test/architecture` holds the import rules. They run inside the
suite, which is why neither has a step of its own.

## Required checks

Six job names are required status checks on `main`:

```text
the Go tree, its tests and its generated files
workflows, scripts and containers
known vulnerabilities
security analysis
dependency changes
controller image
```

GitHub matches a required check by its name, and the branch requires strict
status checks with `enforce_admins` on, so a renamed job leaves a required
check that never reports and nobody — including an administrator — can merge
until the setting is edited.

Renaming one is three operations in one sitting, in this order: push the rename
and let the run report the new name, replace the old context with the new one in
a single write to the branch protection, then merge. The API takes a context
that has never reported, so the requirement is switched rather than removed, and
the branch is never without it. What the window costs is that any other open
pull request, still carrying the old name, cannot merge until it picks up the
rename.

## Names

**A job is named for the subject its gate guards**, not for the tool that
inspects it and not for one of the acts it performs. The name is what a reader
sees in the checks list, next to five others, so it has to say which part of the
tree just passed. A job id is an identifier rather than a label: it is short,
it appears in `needs:`, and it does not have to match the name.

**A step is named as an imperative, in Sentence case, with its article, for the
one act it performs.** `Verify the module is tidy`, not `Module tidy check`;
`Verify the image runs`, not `The image must run`. A check uses `Verify` unless
the tool's own word is the act, as in `Lint the workflows`.

**One act has one name.** The capsule suite, the lifecycle drills and the
durability suite each run in two workflows, and the names have to match; where
they drifted, the pull request that ran them side by side is what found it. A
step that does two things gets two names, which is why closing the registry
session is its own step rather than a clause inside a cleanup check.

**A script is named for its subject, under a directory named for its verb or
its domain**: `scripts/verify/coverage.sh`, `scripts/contracts/docker.sh`. Two
levels, always — `test/consistency` globs `scripts/*/*.sh`, and a script at the
top level leaves that check without anything saying so. An imperative name is
reserved for a script another program invokes as a command, which is why
`scripts/qualification/start-jit-runner.sh` keeps its verb.

**A pair of scripts splits by machine.** The driver you run from your own
machine lives under `scripts/`; the half it ships to the host under test lives
under `test/`, named `remote-harness.sh`.

Every script carries `#!/usr/bin/env bash`, `set -euo pipefail`, a header saying
what it proves and why it exists, a `# Usage:` line, and the executable bit. All
of them are shellchecked, at any depth, tracked or not.

## Shell, and what is not shell

A script is shell when its job is to run other programs: `ssh`, `tar`, `docker`,
`uname`. It is a Go test or a Go command when its job is to read a file and
decide something — those get parsed with the parser for the format, and they get
tests of their own. The repository does not add a second language toolchain for
this: nothing here is written in Python.
