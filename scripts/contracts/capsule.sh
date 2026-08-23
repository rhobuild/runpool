#!/usr/bin/env bash
set -euo pipefail
# Run the outer-capsule contract suite against a real daemon over SSH:
# build the capsule image from the pinned inputs on the host, ship the
# cross-compiled suite, and drive the whole supervisor lifecycle with a
# fake credential — the real runner runs and holds it, and the drain is
# what ends the serving, which proves the runner ran without needing a
# provider.
#
# Usage: scripts/contracts/capsule.sh <ssh-host>
#   RUNPOOL_CONTRACT_HOST  default for <ssh-host>
cd "$(dirname "$0")/../.."
host=${1:-${RUNPOOL_CONTRACT_HOST:-}}
if [ -z "$host" ]; then
  echo "usage: scripts/contracts/capsule.sh <ssh-host>   (a Linux Docker host)" >&2
  exit 2
fi
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

echo "== cross-compile linux/amd64 (CGO_ENABLED=0) =="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$out/capsule-contract.test" ./test/contract/capsule

echo "== ship the tree and build the capsule image on $host =="
COPYFILE_DISABLE=1 git ls-files -coz --exclude-standard | tar --no-xattrs -czf "$out/src.tgz" --null -T - 2>/dev/null
remote_dir=$(ssh "$host" 'mktemp -d /tmp/runpool-capsule-contract.XXXXXX')
remote_dir_q=$(printf '%q' "$remote_dir")
trap 'rm -rf "$out"; ssh "$host" "rm -rf $remote_dir_q" || true' EXIT
# The harness goes with it: the deadline the suite is allowed lives there,
# so this driver and both CI gates give it the same one.
scp -q "$out/src.tgz" "$out/capsule-contract.test" \
  test/contract/capsule/remote-harness.sh "$host":"$remote_dir/"
# shellcheck disable=SC2029 # client-side expansion is intended: remote_dir comes from the remote mktemp above
ssh "$host" "set -e
mkdir -p '$remote_dir/src'
tar -xzf '$remote_dir/src.tgz' -C '$remote_dir/src' 2>/dev/null
cd '$remote_dir/src'
docker build -q -f build/capsule/Dockerfile -t runpool-capsule:dev . >/dev/null
bash '$remote_dir/remote-harness.sh' '$remote_dir'"
