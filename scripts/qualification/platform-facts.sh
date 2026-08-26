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
# findmnt names the filesystem; stat -f prints its magic number, and
# ext2, ext3 and ext4 share one -- so a host running ext4 was frozen into
# this document as "ext2/ext3", and this document exists to be read
# literally.
#
# The path is the daemon's own data root rather than a hardcoded one, so
# a relocated --data-root is followed. What it is not is where layers
# live under the containerd image store: those are under containerd's
# root, which this does not probe. The fact has always named the
# daemon's storage, the two roots are the same filesystem on any
# ordinary host, and widening it to two values is a manifest change --
# said here so the next reader does not mistake this for a probe of the
# image store.
docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
# A tool that is not here is not a filesystem that is not there. Swallowed,
# the absence produced an empty value: the freeze then refused the record as
# incomplete, and a qualification run reported the host's filesystem as ""
# against the reference's ext4 -- a mismatch an operator reads as the wrong
# disk rather than as a missing package. findmnt is util-linux, which every
# glibc distribution installs by default and busybox ones do not.
if ! command -v findmnt >/dev/null 2>&1; then
  echo "platform-facts: findmnt not found; install util-linux to collect the backing filesystem" >&2
  exit 1
fi
backing=$(findmnt -no FSTYPE --target "$docker_root")
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
