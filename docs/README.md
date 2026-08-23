# Runpool documentation

Documentation is organized by the question it answers.

## Understand the system

- [Product contract](product-contract.md): guarantees, exclusions, and
  compatibility surfaces.
- [Architecture](architecture.md): component boundaries, durable identity,
  recovery, capacity, and storage.
- [Threat model](security/threat-model.md): trust boundary, defences, one
  known weakness, and accepted exposure.
- [Architecture decision records](adrs/README.md): decisions that constrain
  implementation and operations.

## Operate Runpool

- [Deployment](deployment.md): the Compose contract, what a platform has to
  provide, credentials, upgrades, and removal.
- [Compose configuration example](../deploy/compose/config.example.yaml):
  multi-target file-mode deployment without embedded credentials.
- [Runbook](runbook.md): startup, pressure response, backup, restore, upgrade,
  rollback, cleanup, and uninstall.
- [Configuration reference](reference/configuration.md): settings, defaults,
  and validation.
- [CLI reference](reference/cli.md): generated command and flag reference.
- [Status API](reference/status-api.md): structured status contract.
- [Support matrix](reference/support-matrix.md): runtime compatibility,
  release-reference platform, and provider
  scope.

## Release Runpool

- [Release readiness](release-readiness.md): objective gates that must be
  satisfied before the first release.
- [`build/platform.lock.json`](../build/platform.lock.json): reviewed platform
  selection and, once frozen, exact facts required by qualification.
- [`build/images.lock.json`](../build/images.lock.json): digest-pinned capsule
  inputs.
- [Continuous integration](maintainers/ci.md): what each gate proves, where it
  runs, and the names the workflows use.
- [Repository settings](maintainers/repository-settings.md): GitHub rulesets,
  environments, Actions permissions, and security features required before
  public launch.
- [Qualification host](maintainers/qualification-host.md): building, freezing,
  and destroying the machine the live gates are answered on.
- [Security review scope](security/review-scope.md): where the release boundary
  runs, the evidence that already exists, and what a review must answer.
- [Security review findings](security/review-findings.md): the register that
  keeps the release authorization gate closed until findings are closed.

The [security policy](../SECURITY.md), [support policy](../SUPPORT.md), and
[contribution guide](../CONTRIBUTING.md) live at the repository root so GitHub
can surface them automatically.
