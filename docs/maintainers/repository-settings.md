# Repository settings

The repository files define CI behaviour, but GitHub security controls also
require organization and repository settings. Maintainers should apply and
review this baseline before making the repository public.

## Rulesets

Protect `main` with a repository ruleset that:

- requires pull requests, resolved conversations, and dismissal of stale
  approvals;
- requires the `ci` and `codeql` workflow checks;
- requires signed commits, and blocks force pushes and branch deletion;
- allows the merge commit as the only merge method; and
- restricts bypass to the Rhobuild maintainer team.

Rebase merging replays commits without signing them, and squashing discards the
branch's own messages. Only the merge commit keeps both, so linear history is
not required: it forbids the one method that works.

Whether that ruleset also requires an approving review depends on how many
maintainers there are, and the two settings are stated here so neither reads as
an oversight.

**With more than one maintainer**, set `required_approving_review_count` to 1
and require CODEOWNERS approval. Every path already has an owner, and an owner
who did not write the change can answer for it.

**With one maintainer**, set both off. GitHub does not allow anyone to approve
their own pull request — no permission level changes that, including
organization ownership — so a lone maintainer who requires an approving review
can merge only by bypassing the rule. A bypass taken on every merge stops
distinguishing the routine one from the one that should have given somebody
pause, which leaves the branch less protected than an honest setting would.

What holds `main` in that case is everything else: the required checks, signed
commits, and conversations that must be resolved. CODEOWNERS
still requests review on every pull request and still records who answers for
each path; it simply stops being a lock only one person can close and only that
same person can force. The approval that does exist is the `release`
environment's, which permits self-review and gates the only irreversible act —
publishing a release.

Raise the setting as soon as the maintainer team gains a second member.

Do not require the path-filtered Docker integration workflow for every pull
request; documentation-only changes do not create that check. Require its
successful result through review policy whenever a changed path triggers it.

Protect `v*` tags from update and deletion and restrict tag creation to release
maintainers. A release workflow is only as trustworthy as the tag that starts
it.

## Actions and environments

- Set the default workflow token to read-only and disable workflows creating
  or approving pull requests.
- Allow only GitHub-authored actions and the explicitly reviewed third-party
  actions pinned by full commit SHA in this repository.
- Keep Dependabot enabled so pinned actions and modules receive reviewed
  update pull requests.
- Put the release-qualification runner in a dedicated runner group restricted
  to this repository. Use labels `self-hosted`, `linux`, `x64`, and
  `runpool-release-qualification`, with Actions Runner 2.327.1 or newer. It must not host
  services or production credentials, must never accept pull-request jobs, and
  must be reprovisioned after each qualification run.
- Protect `release-qualification` with required release-maintainer approval. Approval
  is required before any selected capsule digest reaches the privileged
  self-hosted host. The workflow rejects non-tag refs; reviewers must still
  verify the protected SemVer tag plus both image digests before approval.
- In `release-qualification`, configure a dedicated private E2E fixture
  repository whose own `runpool-e2e.yml` workflow is byte-for-byte equal to
  `test/e2e/controller/testdata/workload.yml`. Set
  `RUNPOOL_E2E_ORGANIZATION`, `RUNPOOL_E2E_REPOSITORY`,
  `RUNPOOL_E2E_GIT_REVISION`, and
  `RUNPOOL_E2E_APP_CLIENT_ID` as Environment variables and
  `RUNPOOL_E2E_APP_PRIVATE_KEY` as an Environment secret. Install the App only
  on that repository with Actions and Administration write, Contents read, and
  Packages write. The workflow mints a short-lived installation token; do not
  store a personal access token. The fixture workflow removes the image
  version it pushed as its final step, with its own run token — the packages
  API refuses App installation tokens, so the harness cannot do it. A run
  that never completes may leave its version behind; the evidence names the
  tag, and the maintainer removes it with a classic personal token holding
  `read:packages` and `delete:packages`.
- Protect `upstream-contracts` and configure a GitHub App installed on the
  fixture organization and limited to the fixture repositories. Grant only
  repository Actions, Contents and Administration plus organization
  Self-hosted runners permissions required by the contracts.
  Store `RUNPOOL_CONTRACT_APP_CLIENT_ID` and fixture identifiers as Environment
  variables, and `RUNPOOL_CONTRACT_APP_PRIVATE_KEY` as an Environment secret.
  `RUNPOOL_CONTRACT_REPO_A` is also the repository-scoped contract target; no
  separate repository selector is used. Both repository identifiers are
  `owner/name`: the contracts address them as REST paths and the crossover
  proof compares them against the owner and repository the runner reports.
  `RUNPOOL_CONTRACT_RUNNER_CMD` is a command run from the contract package's
  directory, so
  [`scripts/qualification/start-jit-runner.sh`](../../scripts/qualification/start-jit-runner.sh)
  is reached as `../../../scripts/qualification/start-jit-runner.sh`. It starts
  one runner from the digest pinned in the image lock and keeps the JIT bundle
  off the host.
  The workflow mints separate, short-lived repository and organization tokens;
  do not store a personal access token.

  These contracts never run on a pull request, because they reach protected
  fixtures. So a change to `github.com/actions/scaleset` merges on hermetic
  tests alone unless somebody dispatches them: Dependabot keeps that update out
  of the grouped batch, CODEOWNERS holds `go.mod`, and the reviewer runs
  `contracts-github-actions` by hand and links the run before merging.
- Link the `runpool` and `runpool/capsule` container packages to this
  repository with the Write role, from each package's own Actions access
  settings. A package created outside a workflow — by a person pushing from a
  laptop — carries no repository link, and this repository's `GITHUB_TOKEN`
  cannot write to it: the release builds the image and then fails at its first
  push with `denied: permission_denied: write_package`. There is no API for the
  grant and none for reading it back: the packages API reports a repository only
  for a package a workflow published, so the only confirmation that the grant
  took is a push that succeeds. Dispatching `release.yml` by hand is that push,
  and it stops before qualifying or publishing anything.
- Make both packages public before the first release. The repository is public;
  a release whose images nobody can pull is not one.
- Protect `release-candidate` with release-maintainer approval. It gates the
  one job that writes to the registry before anything has been qualified: the
  candidate images a release later promotes. The protected `v*` tag is what
  authorizes the run, and this is where that authorization is recorded and can
  be held. With a single maintainer, self-review applies here for the reason
  the rulesets section gives: a bypass taken on every run protects nothing.
- Protect `release` with required maintainers and the external security review
  approval. Only the final publish job may use it.

## Security features

Enable the dependency graph, Dependabot alerts and security updates, CodeQL
code scanning, secret scanning with push protection, and private vulnerability
reporting. Retain artifact attestations and package provenance for every
release.

Review collaborators, deploy keys, webhooks, installed Apps, environments,
ruleset bypasses, and organization secrets at least quarterly and after any
maintainer change.
