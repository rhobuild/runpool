# Deployment

Runpool ships one Compose contract. Every installation is headless: the
controller has no inbound application endpoint, so it needs no domain,
reverse-proxy route, or published port.

Runpool is pre-release. The manifests require an explicit image reference and
there is no released digest yet. A deployment becomes supported only after its
exact image and host platform have passed the gates in
[release readiness](release-readiness.md).

## Deployment surface

[`deploy/compose/compose.yaml`](../deploy/compose/compose.yaml) is the canonical
operator-managed deployment. Platform catalogs derive their template from this
contract; they are not duplicated here because an unreviewed second copy would
drift.

The Dokploy One-Click template follows the
[official contribution workflow](https://dokploy.github.io/templates/): fork
`Dokploy/templates`, add `blueprints/runpool`, open a pull request, test its
automatically generated preview and Base64 import, and let the upstream merge
become the catalog authority. Do not submit a mutable development image. The
blueprint is created from an immutable release candidate or release and updated
through an upstream pull request whenever the deployment contract changes.

## Choose the host topology

The controller has full Docker API authority even though the socket file is
mounted read-only, and each job capsule runs a privileged inner daemon. The
topology is therefore explicit rather than inferred:

| Topology | Use it when | Contract |
| --- | --- | --- |
| `shared-daemon` | Dokploy and application services already use this Engine | Private, trusted CI only; restricted egress; explicit host reserve; no platform-wide volume prune |
| `dedicated-daemon` | The Engine is exclusive to Runpool | Smaller compromise blast radius and the recommended security posture; still not a hostile-code sandbox |

`shared-daemon` lets the operator run and observe the controller and ephemeral
capsules through the same Docker control plane. It does not make Docker
multi-tenant: controller, daemon or kernel compromise can affect Dokploy and
every colocated service. Use it only when that impact is an accepted risk for
the workflows being admitted.

Runpool labels its controller-visible objects with `io.runpool.*` ownership
and correlation fields. Cleanup re-inspects instance and lease ownership
before deleting each recorded object. It never removes unrelated resources and
never issues daemon-wide prune.

### Shared-daemon cleanup policy

Disable Dokploy jobs or maintenance actions that run any of the following
against the shared Engine:

- `docker volume prune`;
- `docker system prune --volumes`;
- an equivalent “full prune” or unused-volume cleanup.

An idle cache lane is intentionally unattached, so Docker considers it unused
even though Runpool plans to reuse it. Runpool's disk monitor and `runpool gc`
own cache eviction. Container, network and image cleanup that excludes volumes
does not delete cache state; missing immutable images are pulled again by
digest. If an idle Runpool uplink network is removed, the controller re-proves
and recreates it before the next capsule is admitted.

## Immutable images

Set `RUNPOOL_IMAGE` to the complete controller reference published by the
release, including `@sha256:<digest>`. The controller binary is built with the
exact capsule digest it is allowed to launch. Tags are discovery aids; they are
not deployment identity, and `latest` is never accepted as an operator choice.

Before starting the controller, verify:

```text
runpool version
runpool config effective
runpool doctor
```

Do not override a failed preflight. The exact V1 release-qualification target is in the
[support matrix](reference/support-matrix.md).

## Verify the release provenance

Publication is attested, and it runs only after qualification has passed
against the same digests. An attestation signed by the release workflow at a
release tag therefore says more than "GitHub built this": it says these exact
bytes are the ones the gates were answered on. Verify before a digest reaches a
deployment.

`--repo` and `--signer-workflow` bind the attestation to this repository's
release workflow rather than to any workflow anywhere, and `--source-ref` binds
it to the release being deployed:

```bash
gh attestation verify oci://ghcr.io/rhobuild/runpool@sha256:<digest> \
  --repo rhobuild/runpool \
  --signer-workflow rhobuild/runpool/.github/workflows/release.yml \
  --source-ref refs/tags/<version> \
  --deny-self-hosted-runners
```

Repeat it for `ghcr.io/rhobuild/runpool/capsule@sha256:<digest>`, which
`runpool version --json` reports as `capsule_image`, and for a downloaded
standalone binary by passing its path instead of the image URI.

A release binary reports its version and that digest. One built from
source reports `runpool dev` and `runpool-capsule:dev`, which is the
pairing a development build uses and not something to verify against a
registry.

`--deny-self-hosted-runners` is not decoration. Publication runs on
GitHub-hosted infrastructure, so an attestation claiming a self-hosted origin
did not come from this path. Each image also carries an SBOM attestation,
retrievable with `gh attestation download`.

A failure here is a stop. It means the digest is not the one this release
qualified, whatever a tag says about it.

## Configuration and provider credentials

The controller runs in file mode. Compose mounts one configuration document at
`/etc/runpool/config.yaml` and one credential directory at
`/run/secrets/runpool`; neither credential values nor target-specific settings
enter the Compose model or the container environment. Start from
[`deploy/compose/config.example.yaml`](../deploy/compose/config.example.yaml).

Each `credentials[].tokenFile` names a file below the mounted credential
directory. Create each source file with mode `0600`, give it a distinct name,
and restrict access to the deployment operator. The controller needs all
configured target credentials, so mounting one read-only directory has the
same authority as mounting those files individually while allowing targets to
be added without changing the deployment manifest.

The default Compose source paths are
`../files/runpool/config.yaml` and `../files/runpool/credentials`. Dokploy
preserves `../files` across deployments, unlike files from a repository clone.
Other Compose operators set `RUNPOOL_CONFIG_PATH` and
`RUNPOOL_CREDENTIALS_PATH` to explicit existing sources. Both bind mounts use
`create_host_path: false`, so a typo fails startup instead of silently creating
an empty directory.

Runpool authenticates with a personal access token or as an installation of
a GitHub App. For a token, prefer a fine-grained one limited to the
configured target and its required Administration or
Self-hosted runners write permission, and verify it with `runpool doctor`;
GitHub documents the accepted alternatives and classic scopes in its
[self-hosted runner authentication requirements](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#authentication-requirements).
Alternatively, authenticate as an installation of a GitHub App, which is
the better credential for a long-running deployment: it belongs to the
organization rather than to a person, and the provider client mints and
refreshes its installation token itself. See `credentials[].type` in the
[configuration reference](reference/configuration.md#credentials).

Runpool reads whichever credential it is given at startup. The platform
control plane is not an external secret manager; rotate a credential
through a controlled controller restart.

## The GitHub side

Runpool creates and owns a **scale set** per (target, tier). A workflow
reaches one by naming it:

```yaml
jobs:
  build:
    runs-on: runpool-standard
```

The label is `tiers[].scaleSetName`, which defaults to `runpool-<tier
id>`. It is a name, not a label set: `runs-on: [self-hosted, linux, x64]`
does not reach a scale set, because GitHub matches one by its name and
nothing else. `runpool doctor` prints the name for every tier a
deployment serves, so the string in configuration and the string in a
workflow can be compared without reading either. A complete example
workflow is in [`deploy/workflows/example.yml`](../deploy/workflows/example.yml).

Before a job can run, the GitHub side has to be true:

| | |
| --- | --- |
| **Target shape** | `https://<host>/<owner>/<repo>` for one repository, `https://<host>/<owner>` for an organization, `https://<host>/enterprises/<name>` for an enterprise. A single-segment URL is an **organization**, not a user account: a personal account has no runner groups and no organization-scoped scale sets. |
| **Runner group** | Required for an organization or enterprise target on a shared daemon, and it must already exist — Runpool resolves a group and never creates one. GitHub displays the built-in group as `Default`; configuration takes it lowercase, as `default`. Custom groups are a paid-plan feature. |
| **Repository access** | The runner group must grant access to the repositories whose workflows will run here. If it does not, GitHub simply never sends work: the controller sits idle and the workflow queues. `runpool status` shows when each binding last reached the provider, which is what tells that apart from having nothing to do. |
| **Actions enabled** | On the repository, and for the workflow's branch. |
| **Credential** | A token with the target's runner administration permission, or a GitHub App installation. `runpool doctor` proves it and names the identity it used. |
| **The scale set** | Runpool creates it on its first serve. Pre-creating one it has no record of is refused, because a set that merely shares a name is a stranger's. |

## Installation sequence

1. Verify the host and Docker daemon against the support matrix.
2. Pull the controller by digest and
   [verify its release provenance](#verify-the-release-provenance).
3. Provision the configuration and one credential file per target through the
   selected platform; verify that every credential source is restricted to the
   deployment operator.
4. Set `host.topology` in the configuration. For `shared-daemon`, size
   `host.reserve` from the measured peak of Dokploy plus colocated production
   services; values in the example are illustrative, not production defaults.
5. Set `scheduling.parallelism` from the host budget. If it is omitted, every
   tier may fill independently and the worst-case maximum is their sum. Enable
   tier swap only when the host provides sufficient encrypted swap and
   `runpool doctor` confirms Docker can enforce it.
6. Disable platform-wide volume cleanup on a shared daemon.
7. Render the complete Compose model and confirm there are no ports, domains,
   proxy labels, or unexpected mounts.
8. Start the controller and wait for its healthcheck.
9. Run `runpool doctor` and `runpool status`; both report topology and scheduling capacity.
10. Execute a private fixture workflow before admitting ordinary CI work —
    one job, `runs-on` naming a tier this deployment serves. If it queues
    without starting, the GitHub side above is where to look, and
    `runpool status` says whether the binding is reaching the provider at
    all.

The default `public-internet-only` profile supports proxy-aware HTTP and HTTPS
traffic. Read [capsule egress](runbook.md#capsule-egress) before evaluating a
workflow that uses SSH or arbitrary TCP.

## Upgrade, backup, and removal

The named state volume is the ownership ledger and must be backed up before an
upgrade. Cache lanes are disposable and are not part of that backup. Upgrade by
replacing the controller digest explicitly, then follow the migration and
rollback procedure in the [runbook](runbook.md#lifecycle-procedures).

Deleting a platform service does not replace `runpool uninstall`. First stop
admission, inspect the dry-run plan, and confirm the instance identifier so the
controller can remove only the resources its state proves it owns. Delete the
platform service and state volume only after the uninstall completes.

## Catalog publication

The Dokploy catalog is maintained upstream, not copied into this repository. A
release maintainer derives the blueprint from the released, digest-qualified
Compose contract and reviews the generated preview. Catalog updates are release
work: a mutable development image or unreleased schema must never be published
as a One-Click service.

## Dokploy without a catalog entry

The public One-Click catalog is not a prerequisite for deployment. Until the
first qualified release is accepted upstream, use this procedure:

1. In the Dokploy organization that owns the server, create project `runpool`,
   environment `production`, and a Docker Compose service named `runpool`.
2. Use the canonical
   [`deploy/compose/compose.yaml`](../deploy/compose/compose.yaml) as the Compose
   definition. It creates only the `controller` service inside the `runpool`
   Compose project.
3. Through **Advanced → Mounts**, create the configuration and credential files
   under Dokploy's persistent `../files` area:
   `../files/runpool/config.yaml`,
   `../files/runpool/credentials/<credential-id>`. Copy the configuration
   example, then replace target names and capacity with reviewed values.
4. Put exactly one token in each credential file, terminate it with an optional
   newline, and set every credential file to mode `0600`. Never put a token in
   the configuration, Compose definition, or Dokploy environment editor.
5. Set only `RUNPOOL_IMAGE` in the environment editor, using the complete
   released `@sha256:` reference. Set `RUNPOOL_CONFIG_PATH` or
   `RUNPOOL_CREDENTIALS_PATH` only when deliberately using non-default source
   paths.
6. Render the Compose model and verify that it contains no token values,
   published ports, domains, or reverse-proxy labels. Validate the mounted
   document with `runpool config effective` before starting `serve`.
7. Deploy, wait for the healthcheck, and run `runpool doctor` and
   `runpool status`. Admit ordinary work only after a private fixture workflow
   succeeds for every target.

The controller is headless: do not attach a domain or expose a port. This keeps
the source of truth in Runpool while following Dokploy's normal Compose path;
the later catalog blueprint is distribution metadata in the upstream template
repository, not a second deployment implementation here.
