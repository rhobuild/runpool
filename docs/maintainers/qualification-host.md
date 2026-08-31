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

That block describes the entry the lock holds today. It is not a rule:
the lock records one entry per platform qualified, each with its own
policy and its own frozen facts, and neither the distribution nor the
architecture is fixed by the code that reads it. Qualifying a second
platform is an added entry rather than a change to the file's shape,
which matters because the file is itself the proof of the gate.

A host with no entry fails as *not qualified on this platform*, naming
the ones that are — a different answer from a host that was measured and
differed. What a release builds for is stated separately in
[`build/images.lock.json`](../../build/images.lock.json), and neither
list promises the other: see the
[support matrix](../reference/support-matrix.md).

What the reader still refuses is a platform no release builds for, and a
reference frozen after the candidate exists. The first would record
evidence about something nobody can run; the second is evidence that
could have been written to fit what it was meant to judge.

## Sizing

Taken from what the suites actually request, not from a guess:

| Suite | Asks for |
| --- | --- |
| Capsule budget contracts | 2 CPU, 768 MiB memory, 256 MiB swap, 512 PIDs |
| Controller end-to-end | 2 CPU and 4 GiB for the tier, 1 CPU and 2 GiB reserved |

Four vCPU and 8 GiB of memory clear both with headroom. Disk needs room for the
runner image, the dind payload, an inner daemon's data root and the images a
workload builds: 40 GB is comfortable.

**Swap must exist.** The scheduling and swap envelope gate proves that
configured swap is used inside a real capsule: the contract drives tmpfs pages
past `memory.max` and requires a non-zero `memory.swap.current`. It also drives
an inner workload into OOM and requires the aggregate capsule cgroup's
hierarchical `oom_kill` counter to increase. `runpool doctor` reads `SwapTotal`
from the kernel. A host without swap cannot pass that gate — the check is not
skipped, it fails.

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

Then the runner agent, version 2.327.1 or newer. It refuses to configure
as root, and this host is administered as root, so it belongs to a user of
its own — `runner`, in the `docker` group, which is also the uid the capsule
drops its workload to:

```bash
useradd -m -s /bin/bash -G docker runner
su - runner -c 'mkdir -p ~/actions-runner && cd ~/actions-runner &&
  curl -fsSL -o runner.tar.gz \
    https://github.com/actions/runner/releases/download/v2.327.1/actions-runner-linux-x64-2.327.1.tar.gz &&
  tar xzf runner.tar.gz && rm runner.tar.gz'
```

Register it with the labels the qualification jobs select on. `config.sh`
supplies the first three itself and not the fourth, and a runner missing it
takes no jobs and reports nothing wrong — the workflows simply queue for a
runner that never answers:

```
self-hosted, linux, x64, runpool-release-qualification
```

```bash
su - runner -c 'cd ~/actions-runner && ./config.sh --url <repository> \
  --token <registration token> --labels runpool-release-qualification --unattended'
cd /home/runner/actions-runner && ./svc.sh install runner && ./svc.sh start
```

Registered against the repository rather than the organization, which is
what confines it: runner groups are an organization-level feature and do
not exist for a repository-scoped runner, so there is no group to put this
one in and nothing else can select it. Never let it accept pull-request
jobs: a pull request is code from outside, and this agent runs it next to
a privileged daemon.

## Capturing and freezing the reference

[The typed platform-facts collector](../../internal/qualification/hostfacts/collect.go)
reports what the host is and decides nothing. The comparison lives elsewhere on
purpose — a collector that also judged would be free to judge in favour of
whatever it found.

```bash
go run ./internal/qualification/cmd/platform-facts
```

`go run` needs a Go toolchain and one `go mod download`, and the
provisioning above installs neither — deliberately, since the CI jobs
bring their own. Capturing facts from a fresh host therefore means either
installing Go there first, or building the collector elsewhere and
copying it over, which needs no toolchain and no network on the host:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o platform-facts \
  ./internal/qualification/cmd/platform-facts
scp platform-facts <host>:/tmp/ && ssh <host> /tmp/platform-facts
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

The controller E2E first runs on GitHub's pinned `ubuntu-24.04` image. That
portable leg keeps the complete assignment, cache, `SIGKILL` adoption and
cleanup path executable even when this host is unavailable. It cannot qualify
the release platform: only the reference leg embeds facts identical to the
frozen lock and to the other live suites.

The freeze and the qualification run happen against the same live host. Tag
after the lock is committed, let `release.yml` drive the qualification, and
only then destroy the machine.

Take a snapshot first. The next qualification reprovisions from it: a clean
host with identical facts, which is what lets the frozen lock keep matching
without a second review.
