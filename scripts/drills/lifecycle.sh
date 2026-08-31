#!/usr/bin/env bash
set -euo pipefail
# The lifecycle drills: install, backup, restore, upgrade (migration),
# rollback (restore), and uninstall — executed against a real Docker
# host, with the real controller image, so the runbook's procedures are
# proven rather than described. Every drill asserts its outcome; a
# procedure that cannot be verified is not a procedure.
#
# The drills need no GitHub credential. State is seeded by a small
# first-party helper that opens the store the way the controller would;
# everything else goes through the shipped CLI in the shipped image.
#
# Usage: scripts/drills/lifecycle.sh <ssh-host>
#   RUNPOOL_CONTRACT_HOST  default for <ssh-host>
cd "$(dirname "$0")/../.."
if [ "$#" -gt 1 ]; then
  echo "usage: scripts/drills/lifecycle.sh <ssh-host>   (a Linux Docker host)" >&2
  exit 2
fi
host=${1:-${RUNPOOL_CONTRACT_HOST:-}}
if [ -z "$host" ]; then
  echo "usage: scripts/drills/lifecycle.sh <ssh-host>   (a Linux Docker host)" >&2
  exit 2
fi
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

echo "== cross-compile the seed helper (linux/amd64, CGO_ENABLED=0) =="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$out/drill-seed" ./test/drills/seed

echo "== ship the tree and the helper =="
COPYFILE_DISABLE=1 git ls-files -coz --exclude-standard | tar --no-xattrs -czf "$out/src.tgz" --null -T - 2>/dev/null
remote_dir=$(ssh "$host" 'mktemp -d /tmp/runpool-drills.XXXXXX')
remote_dir_q=$(printf '%q' "$remote_dir")
trap 'rm -rf "$out"; ssh "$host" "rm -rf $remote_dir_q" || true' EXIT
scp -q "$out/src.tgz" "$out/drill-seed" test/drills/remote-harness.sh "$host":"$remote_dir/"
# shellcheck disable=SC2029 # client-side expansion is intended: remote_dir comes from the remote mktemp above
ssh "$host" "bash '$remote_dir/remote-harness.sh' '$remote_dir'"
