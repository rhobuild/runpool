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
# These two stay swallowed, and the difference from the guard above is
# the whole reason. findmnt missing was a broken instrument reporting a
# false fact -- the disk had a filesystem either way. iptables missing is
# the fact: the host has no iptables, which is true, and reporting it is
# this script's whole job. The same shape as rootless -- a disqualifying
# property the collector states and the judges refuse, at the freeze
# (the manifest requires both non-empty) and at the comparison, where an
# empty version reads as "not installed" rather than as a different tool.
#
# Nothing here execs them either: the ruleset is applied by
# iptables-restore inside the gateway container, from the image this
# build ships, which installs its own. They record the netfilter
# userspace the evidence was gathered under.
#
# Looked for where they live, not only on PATH. Both install into sbin,
# which a service account's PATH commonly omits and a root shell's
# carries, so a PATH-only probe reports no netfilter userspace on a host
# that has both -- and the comparison reads one host as two, as the wrong
# host rather than as the wrong PATH. The directories are searched in the
# order a root PATH lists them, so a host carrying different binaries in
# two of them answers the same whichever account asks. What remains after
# looking is the fact the swallowing above is for.
netfilter_version() {
  local candidate version
  for candidate in "$1" "/usr/local/sbin/$1" "/usr/sbin/$1" "/sbin/$1"; do
    command -v "$candidate" >/dev/null 2>&1 || continue
    # Per candidate, and emitted only once one answers: a wrapper that
    # prints and then fails would otherwise have its half-line captured
    # alongside the next candidate's whole one, and the two arrive in the
    # record as one value with a newline through it.
    version=$("$candidate" --version 2>/dev/null) || continue
    printf '%s' "$version"
    return 0
  done
  return 0
}
iptables_version=$(netfilter_version iptables)
nft_version=$(netfilter_version nft)

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
