# Security policy

## Reporting a vulnerability

Report privately through GitHub's security advisory form on this
repository ("Report a vulnerability" under Security). Please do not open
a public issue for a suspected vulnerability.

Include what you did, what happened, and what you expected. A proof of
concept helps; a full exploit is not required.

You can expect an acknowledgement within a week and an assessment within
two. Runpool is maintained by a small team, so those are honest targets
rather than a contractual response time. If a report is confirmed, we
will agree a disclosure timeline with you and credit you unless you
prefer otherwise.

## Supported versions

Every fix lands on `main`, and reaches an installation through a release.
The most recent release is the one that receives them; a fix is not
backported to an earlier version.

## What is in scope

Anything that breaks a defence the project claims. Those claims, and the
evidence behind each, are in [the threat model](docs/security/threat-model.md).
The most valuable reports concern:

- making Runpool act on a Docker resource it does not own;
- one repository's job reaching another repository's cache lane;
- the long-lived provider credential reaching a job, log, state database, or
  error message; or a per-runner JIT bundle reaching controller persistence,
  Docker metadata, or logs;
- a lease releasing while its resources survive, or an admission credit leaking so
  the host is oversubscribed;
- any bypass of the egress sandbox under the `public-internet-only`
  profile: a capsule reaching a host, LAN, metadata or other-Docker
  address, or reaching anything at all without its gateway.

## What is out of scope

These are documented properties of the design, not vulnerabilities:

- **The controller holds the host Docker socket**, which confers
  host-root authority. Controller compromise is host compromise.
- **Job capsules run a privileged Docker daemon.** A kernel escape from
  inside a capsule reaches the host. Runpool isolates with namespaces
  and policy, not hardware.
- **`shared-daemon` shares that compromise domain with platform and
  application services.** Its reserve and ownership controls protect normal
  coexistence, not a controller, daemon or kernel compromise.
- **Workflows from fork pull requests on public repositories are
  unsupported**, per GitHub's guidance for self-hosted runners.
- **The `unsafe-open-egress` profile filters nothing**, by definition
  and by its name: a deployment that selects it has chosen host-open
  egress, and reports against that profile's reachability are reports
  about the choice, not the code.
- **The restricted profile is not transparent L3 egress.** Direct traffic,
  `git+ssh`, non-DNS UDP, other ports, and proxy-unaware clients failing
  closed is the design. CONNECT to an allowed address on ports 80 or 443 is
  an opaque tunnel and is evaluated by destination and port, not payload.
- **The assigned workload can observe its own JIT bootstrap state.** GitHub's
  supported runner interface requires `--jitconfig`; it is transiently present
  in the runner's argv, and the job runs under the same Unix identity. The
  bundle is ephemeral and authorizes only that runner. Runpool protects it from
  controller persistence, Docker metadata, and later workloads; it does not
  claim secrecy from the workload it starts.

Runpool is a resource and hygiene boundary for CI you already trust. If
a report's premise is that it should contain hostile workflow code, the
answer is in the threat model rather than in a patch.
