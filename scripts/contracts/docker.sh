#!/usr/bin/env bash
set -euo pipefail
# Run the Docker client contract suite against a real daemon over SSH.
# The suite runs on the host rather than inside a container because the
# contract under test is the daemon API itself, and the resource-envelope
# check reads cgroup v2 paths, so a Linux host is required.
#
# Usage: scripts/contracts/docker.sh <ssh-host>
#   RUNPOOL_CONTRACT_HOST     default for <ssh-host>
#   RUNPOOL_CONTRACT_QUALIFY  non-empty: qualification mode — the suite
#                             additionally requires the exact platform
#                             named in build/platform.lock.json.
#
# The expectation comes from the reviewed manifest, never from the host under
# test.
cd "$(dirname "$0")/../.."
host=${1:-${RUNPOOL_CONTRACT_HOST:-}}
if [ -z "$host" ]; then
  echo "usage: scripts/contracts/docker.sh <ssh-host>   (a Linux Docker host)" >&2
  exit 2
fi
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

echo "== cross-compile linux/amd64 (CGO_ENABLED=0) =="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$out/docker-contract.test" ./test/contract/docker

echo "== host under test =="
ssh "$host" 'docker version --format "engine {{.Server.Version}} api {{.Server.APIVersion}}"; docker info --format "cgroup {{.CgroupVersion}} driver {{.CgroupDriver}} security {{.SecurityOptions}}"'
echo "== release-qualification reference (build/platform.lock.json) =="
grep -E '"(engine|api|os|os_version|arch|cgroup_driver)"' build/platform.lock.json

echo "== contract suite on $host =="
# The remote directory is run-scoped: a fixed path lets two runs — or
# two operators — overwrite each other's binaries mid-test. printf %q
# quotes it portably; ${var@Q} needs bash 4.4, which macOS lacks.
remote_dir=$(ssh "$host" 'mktemp -d /tmp/runpool-docker-contract.XXXXXX')
remote_dir_q=$(printf '%q' "$remote_dir")
trap 'rm -rf "$out"; ssh "$host" "rm -rf $remote_dir_q" || true' EXIT
# The harness goes with it: clearing the pull fixture and proving it gone
# is the suite's precondition, and both CI gates run the same file rather
# than a second spelling of it.
scp -q "$out/docker-contract.test" test/contract/docker/remote-harness.sh "$host":"$remote_dir/"
qualify_env=""
if [ -n "${RUNPOOL_CONTRACT_QUALIFY:-}" ]; then
  # No expected version travels from here: the suite embeds the
  # reviewed manifest and compares the host against it.
  qualify_env="RUNPOOL_CONTRACT_QUALIFY=1"
fi
# shellcheck disable=SC2029 # client-side expansion is intended: remote_dir comes from the remote mktemp above
ssh "$host" "$qualify_env bash '$remote_dir/remote-harness.sh' '$remote_dir'"
