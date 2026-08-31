#!/usr/bin/env bash
set -euo pipefail
# Qualify the pinned SQLite driver: race pass on the local machine, then
# the durability suite on a Linux Docker named volume, including
# container-kill rounds, via the remote harness under
# test/contract/sqlite/testdata/.
#
# Usage: scripts/contracts/sqlite.sh <ssh-host>
#   RUNPOOL_CONTRACT_HOST  default for <ssh-host>
cd "$(dirname "$0")/../.."
if [ "$#" -gt 1 ]; then
  echo "usage: scripts/contracts/sqlite.sh <ssh-host>   (a Linux Docker host)" >&2
  exit 2
fi
host=${1:-${RUNPOOL_CONTRACT_HOST:-}}
if [ -z "$host" ]; then
  echo "usage: scripts/contracts/sqlite.sh <ssh-host>   (a Linux Docker host)" >&2
  exit 2
fi
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

echo "== local race pass =="
mkdir "$out/sqlite-local"
RUNPOOL_SQLITE_CONTRACT_DIR="$out/sqlite-local" go test -race -count=1 -v ./test/contract/sqlite

echo "== cross-compile linux/amd64 (CGO_ENABLED=0) =="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$out/sqlite-contract.test" ./test/contract/sqlite

echo "== remote named-volume suite on $host =="
# Run-scoped remote directory: a fixed path lets two runs overwrite each
# other's binaries mid-test. printf %q quotes it portably; ${var@Q}
# needs bash 4.4, which macOS lacks.
remote_dir=$(ssh "$host" 'mktemp -d /tmp/runpool-sqlite-contract.XXXXXX')
remote_dir_q=$(printf '%q' "$remote_dir")
trap 'rm -rf "$out"; ssh "$host" "rm -rf $remote_dir_q" || true' EXIT
scp -q "$out/sqlite-contract.test" test/contract/sqlite/testdata/remote-harness.sh "$host":"$remote_dir/"
# shellcheck disable=SC2029 # client-side expansion is intended: remote_dir comes from the remote mktemp above
ssh "$host" "bash '$remote_dir/remote-harness.sh' '$remote_dir'"
