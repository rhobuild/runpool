# Threat model

This states what Runpool defends, what it does not, and why. An auditor
should be able to disagree with the boundary here rather than guess at
it. Where a defence is claimed, the evidence for it is named; where the
evidence is a suite that has not run in release qualification, the claim says
"tested live", never "release-qualified".

## What Runpool is

A controller that watches GitHub Actions scale sets and creates one
ephemeral execution capsule per job on a single Docker host. A capsule
is **one container**: a first-party supervisor as PID 1, the runner, and
the job's own Docker daemon, all inside one aggregate cgroup, with
per-job volumes and an optional persistent cache lane. Under the
restricted network profile the capsule has no route out; its egress is a
per-capsule gateway that relays under a default-deny policy.

## Assets

| Asset | Why it matters |
|---|---|
| Provider credential | Administers runners on the target; it remains in the controller |
| Per-runner JIT bundle | Short-lived bootstrap material for one ephemeral runner; the assigned workload shares its execution identity |
| The host Docker socket | Confers host-root authority |
| The state database | Ownership records; deciding what Runpool may delete |
| Cache lanes | Build state reused across jobs of one repository |
| Job workspaces and secrets | Whatever a workflow is given at runtime |
| The host's other networks | The LAN, metadata services, and any other Docker network on the daemon |

## Trust boundary

```text
trusted:   the operator, the host, the controller, the configuration
semi:      workflow code in the operator's own private repositories
untrusted: everything a workflow downloads and executes at runtime
```

Runpool V1 is a **resource-management, environment-hygiene and
network-policy boundary for CI the operator already trusts**. It is not
a sandbox for hostile code, and the documentation says so in the same
words wherever the product is deployed.

The distinction that matters: a workflow author who is trusted still
runs untrusted third-party code — dependencies, actions, containers.
Runpool's per-job disposal and egress policy exist for that layer.

## Deployment topologies

Runpool requires an explicit host topology:

- **`shared-daemon`** is the Dokploy and single-server coexistence contract.
  It is supported only for private workflows the operator already authorizes
  on that host. Runpool withholds an explicit CPU, memory and free-disk reserve,
  requires restricted egress, isolates organization targets in an explicit
  runner group, and proves ownership before deleting an object. Those controls
  protect capacity and lifecycle; they do not protect colocated services from
  controller, daemon or kernel compromise.
- **`dedicated-daemon`** gives Runpool exclusive operational ownership of the
  Engine and is the recommended boundary when CI provenance is broader or the
  impact of colocated service compromise is unacceptable. It still is not a
  VM or hostile-code sandbox.

Both topologies reject public fork workflows as a supported use. A trusted
workflow can download untrusted dependencies, so per-job disposal and egress
policy remain necessary in either topology.

## Defences and their evidence

| Defence | Mechanism | Evidence |
|---|---|---|
| No state survives a job | Fresh dind data root, workspace and control tmpfs per capsule; the data root is a volume removed with the lease | Capsule contract, live |
| A leaked runner cannot outlive its job | Ephemeral JIT runner, one job, removed on failure paths too | Live JIT flow, including removing a runner that never started |
| Provider credentials never enter capsules; JIT state does not persist across jobs | Provider token remains in the controller. JIT arrives over exec stdin, its files are redirected to tmpfs, and it is absent from Docker configuration, environment, labels, and logs. The upstream runner requires it transiently in argv, visible to the assigned workload | Capsule contract asserts volatile materialization and no log disclosure; controller end-to-end qualification remains required |
| Cross-repository cache contamination | Lanes are daemon-side named volumes, exclusive per lease, named by opaque ids, reused only for the same repository and generation | Live lane contract: marker persists for the same lane, another generation is blind |
| Runpool deletes only what it owns | Instance- and lease-scoped ownership labels on every created object; creation recovery and destructive intent cleanup re-inspect ownership before acting; no daemon-wide prune | Foreign-resource contracts pass live; controller E2E asserts unrelated container, network and volume sentinels and still needs release qualification |
| A crashed controller leaves nothing | Adoption of running capsules, sweep of orphans, singleton flock | Live SIGKILL mid-capsule, successor adopted and cleaned |
| The host cannot be starved by a job | One envelope per lease, split between the capsule and its egress gateway and placed under one parent cgroup: the capsule's aggregate covers runner, daemon and every inner container; the gateway holds the rest of the same tier, because every connection a job opens is work it performs. The doctor refuses a configuration whose full tiers plus reserve exceed the host, and the validator refuses a tier too small to split | Kernel-proven on the reference host: inner OOM charged to the capsule; both containers report one parent cgroup and their limits sum to the tier; a fork storm in the gateway stops at its own ceiling |
| The host cannot be filled | Disk monitor probes the daemon's filesystem from inside it; admission closes at the soft floor, fails closed at the hard floor, and GC evicts only free lanes | Pressure transitions table-tested; disk-full behaviour live for both SQLite and containers |
| Egress confinement | Under `public-internet-only`, the host kernel drops anything the capsule addresses beyond its bridge; the gateway relays DNS and HTTP(S) under default-deny, resolving before dialing (rebinding defence) | Live bypass suite: no direct route anywhere, denied addresses refused by name and by address, gateway loss removes egress |

## Accepted exposure

These are consequences of the design, not oversights:

- **Controller compromise is host compromise.** It holds the Docker
  socket. Nothing inside the container — read-only root, dropped
  capabilities, no shell — changes that; they raise the cost of using a
  foothold, not of having one.
- **A privileged dind is not a VM.** A kernel escape from inside a
  capsule reaches the host. Runpool's isolation is namespace and
  policy, not hardware. On `shared-daemon`, the operator explicitly accepts
  that kernel-level compromise can reach colocated services; use
  `dedicated-daemon` when that impact is unacceptable.
- **Platform-wide volume prune bypasses Runpool's ownership model.** Docker
  treats an unattached cache lane as unused. On a shared daemon, disable
  unused-volume cleanup and any system prune that includes volumes. Image
  pruning is compatible: immutable images are pulled again when absent.
- **Fork pull requests from public repositories are out of scope**, per
  GitHub's own guidance for self-hosted runners. Configuration cannot
  express them safely, and the documentation refuses them rather than
  implying containment.
- **The assigned workload can observe its own JIT bundle.** GitHub's runner
  interface requires `--jitconfig`, and the job shares the runner's Unix
  identity. The bundle is one-run and short-lived. Runpool prevents it from
  reaching controller persistence, Docker metadata, logs, or a later workload;
  it does not claim secrecy from the workload it authorizes.
- **Only proxy-aware traffic leaves a sandboxed capsule, and CONNECT is
  a tunnel.** Direct TCP cannot leave — the host drops it — but a
  proxy-aware client can tunnel any protocol over CONNECT to an allowed
  address, on an allowed port. The port set is explicit (443 and 80);
  everything else is refused with a reason. The relay does not inspect
  what flows inside a tunnel and does not claim to. `git+ssh` and
  arbitrary TCP on other ports fail closed; a deployment that needs
  them must choose `unsafe-open-egress`, whose name is its warning
  label.

## Supply chain

- Runtime images are pinned by digest in `build/images.lock.json`; the
  capsule image is built from those pinned inputs and both Dockerfiles
  copy sources by allowlist.
- A tier may run its jobs in an operator-supplied capsule
  (`tiers[].capsuleImage`, digest-qualified, built from the published
  one). That image is outside this chain by the operator's own decision,
  and `runpool status` reports which image each tier runs. **The egress
  gateway is not affected**: the container that applies the network
  policy always runs the capsule this build ships, so extending what a
  job runs in never replaces what confines it.
- The controller is a static binary on distroless with no shell.
- `actions/scaleset` is a Public Preview dependency: pinned, with live
  contract tests that fail when upstream behaviour drifts.
- The SQLite driver is CGo-free and covered by a durability suite — WAL
  behaviour, contention, kill-recovery rounds, disk-full on a capped
  filesystem — that runs against a Linux named volume. No release-qualification
  record exists yet; the protected release workflow is what will produce one.

## What an auditor should attack first

Ranked by what would hurt most if it is wrong:

1. **Ownership scoping.** Can any input make Runpool delete a resource
   it does not own? The labels and the instance id are the only guard.
2. **Egress.** The bypass suite is the starting point, not the finish
   line: DNS tricks through the relay, proxy parsing, the gateway's own
   INPUT surface, and anything that makes a capsule's packet cross the
   host with a spoofed source.
3. **Credential containment.** Does the provider token leave the controller,
   or does JIT state reach Docker metadata, a log, the database, disk-backed
   capsule state, or a later workload?
4. **Cache lane isolation.** Can one repository's job reach another's
   lane — through the lease machine, label spoofing, or reconciliation
   after a crash? (There are no paths left to traverse; the volume
   namespace and the store are the surfaces.)
5. **Lease state machine.** Can a lease release while resources
   survive, or an admission credit leak so the host is oversubscribed?
