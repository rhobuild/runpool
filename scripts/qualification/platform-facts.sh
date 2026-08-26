#!/usr/bin/env bash
set -euo pipefail
# Emit this host's platform facts as JSON for the release-qualification record.
#
# It reports what the host is, and says nothing about whether that is
# right: the comparison against build/platform.lock.json belongs to the
# contract suite, which embeds the reviewed manifest. Keeping the two
# apart is the point — a collector that also decided would be free to
# decide in favour of whatever it found.
#
# Usage: scripts/qualification/platform-facts.sh
# shellcheck source=/dev/null # a host file, not a repository one
os_id=$(. /etc/os-release && printf '%s' "$ID")
# shellcheck source=/dev/null
os_version=$(. /etc/os-release && printf '%s' "$VERSION_ID")
# shellcheck source=/dev/null
os_codename=$(. /etc/os-release && printf '%s' "${VERSION_CODENAME:-}")

engine=$(docker version --format '{{.Server.Version}}')
api=$(docker version --format '{{.Server.APIVersion}}')
cgroup_version=$(docker info --format '{{.CgroupVersion}}')
cgroup_driver=$(docker info --format '{{.CgroupDriver}}')
storage=$(docker info --format '{{.Driver}}')
containerd=$(docker info --format '{{.ContainerdCommit.ID}}')
runc=$(docker info --format '{{.RuncCommit.ID}}')
security=$(docker info --format '{{.SecurityOptions}}')
rootless=false
case "$security" in *rootless*) rootless=true ;; esac

buildx=$(docker buildx version 2>/dev/null | awk '{print $2}' || true)
compose=$(docker compose version --short 2>/dev/null || true)
# The daemon says where its root is, and findmnt names the filesystem
# rather than its magic number: stat -f prints "ext2/ext3" for ext4,
# because the three share one magic, and this value is frozen into a
# document whose whole purpose is to be read literally. A hardcoded
# /var/lib/docker would also aim at the graphdriver root, which is not
# where layers live once the containerd image store is the driver.
docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
backing=$(findmnt -no FSTYPE --target "$docker_root" 2>/dev/null || true)
iptables_version=$(iptables --version 2>/dev/null || true)
nft_version=$(nft --version 2>/dev/null || true)

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) arch=$(uname -m) ;;
esac

cat <<JSON
{
  "os": "${os_id}",
  "os_version": "${os_version}",
  "os_codename": "${os_codename}",
  "arch": "${arch}",
  "kernel": "$(uname -r)",
  "engine": "${engine}",
  "api": "${api}",
  "cgroup_version": "${cgroup_version}",
  "cgroup_driver": "${cgroup_driver}",
  "storage_driver": "${storage}",
  "backing_filesystem": "${backing}",
  "rootless": ${rootless},
  "containerd": "${containerd}",
  "runc": "${runc}",
  "buildx": "${buildx}",
  "compose": "${compose}",
  "iptables": "${iptables_version}",
  "nftables": "${nft_version}",
  "collected_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON
