# The release-qualification host

[Release readiness](../release-readiness.md) names the gates a release must
pass. Four of them can only be answered by a kernel, and this document builds
the machine that answers them.

It is not a machine you keep. It exists for one qualification run, holds no
service and no production credential, and is destroyed afterwards. That is a
security property, not tidiness: during the run it executes CI workloads with a
privileged Docker daemon inside them.

## Why not an existing host

The controller end-to-end gate proves that cleanup removes every owned resource
**and preserves unrelated container, network and volume sentinels by exact id**.
On a host that already runs services, those sentinels are that host's real
work. A test written to catch "Runpool destroys its neighbours" would be using
production as the canary.

## The platform

[`build/platform.lock.json`](../../build/platform.lock.json) states the
selection policy, and `internal/platform` refuses anything else:

```
debian 13 trixie · amd64
Docker Engine from the official stable channel
https://download.docker.com/linux/debian
```

Architecture is checked against the lock, which records one policy and
names `amd64` in it. A host of another architecture fails the comparison
for that reason — not because the suites would fail on it. Recording
several qualified platforms side by side is the subject of the
[multiplatform locks decision](../adrs/2026-08-17-multiplatform-locks.md),
accepted with its implementation pending; until it lands, the lock holds
exactly one platform and the
[support matrix](../reference/support-matrix.md) states what has been
observed and what has not.

## Sizing

Taken from what the suites actually request, not from a guess:

| Suite | Asks for |
| --- | --- |
| Capsule budget contracts | 2 CPU, 2 GiB memory, 256 MiB swap, 512 PIDs |
| Controller end-to-end | 2 CPU and 4 GiB for the tier, 1 CPU and 2 GiB reserved |

Four vCPU and 8 GiB of memory clear both with headroom. Disk needs room for the
runner image, the dind payload, an inner daemon's data root and the images a
workload builds: 40 GB is comfortable.

**Swap must exist.** The scheduling and swap envelope gate proves that
configured swap is enforced inside a real capsule, and `runpool doctor` reads
`SwapTotal` from the kernel. A host without swap cannot pass that gate — the
check is not skipped, it fails.

## Provisioning

Install Docker from the official channel rather than the distribution's, so the
engine matches the policy:

```bash
apt-get update && apt-get install -y ca-certificates curl
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
printf 'deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable\n' \
  > /etc/apt/sources.list.d/docker.list
apt-get update && apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
```

**Hold the engine and disable unattended upgrades.** The lock freezes this
host's exact facts, and the qualification run compares against them. An engine
or kernel patch applied between the freeze and the run turns a passing gate
into a mismatch:

```bash
apt-mark hold docker-ce docker-ce-cli containerd.io
systemctl disable --now unattended-upgrades 2>/dev/null || true
```

Then the runner agent, version 2.327.1 or newer, registered to this repository
with the labels the qualification jobs select on:

```
self-hosted, linux, x64, runpool-release-qualification
```

Put it in a runner group restricted to this repository, and never let it accept
pull-request jobs: a pull request is code from outside, and this agent runs it
next to a privileged daemon.

## Capturing and freezing the reference

[`scripts/qualification/platform-facts.sh`](../../scripts/qualification/platform-facts.sh)
reports what the host is and decides nothing. The comparison lives elsewhere on
purpose — a collector that also judged would be free to judge in favour of
whatever it found.

```bash
scripts/qualification/platform-facts.sh
```

Read the eighteen facts. This is the reviewed step the gate asks for, not a
formality: engine patches, cgroup drivers, storage drivers and backing
filesystems all change container behaviour, which is why each is recorded
rather than summarised.

Then write them into `build/platform.lock.json` with `status` set to `frozen`
and `recorded` set to the review date, copy the file byte for byte to
`internal/platform/platform.lock.json`, and commit. A frozen manifest missing
any fact is rejected, and the embedded and reviewed copies are compared by a
test.

## The run, and after it

The freeze and the qualification run happen against the same live host. Tag
after the lock is committed, let `release.yml` drive the qualification, and
only then destroy the machine.

Take a snapshot first. The next qualification reprovisions from it: a clean
host with identical facts, which is what lets the frozen lock keep matching
without a second review.
