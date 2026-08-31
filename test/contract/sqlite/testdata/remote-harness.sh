#!/usr/bin/env bash
set -euo pipefail
# Remote half of the SQLite durability qualification: runs the suite
# inside a resource-limited container against a Docker named volume, then
# SIGKILLs whole writer containers and verifies recovery. It touches only
# the resources it creates, all named after this run.
#
# Usage: remote-harness.sh <dir>
#   <dir>  run-scoped directory holding sqlite-contract.test; created and
#          removed by the invoking script, never by this one.
if [ "$#" -ne 1 ]; then
  echo "usage: remote-harness.sh <dir with sqlite-contract.test>" >&2
  exit 2
fi
dir=${1:?usage: remote-harness.sh <dir with sqlite-contract.test>}
run_id=$(basename "$dir" | tr -dc 'a-zA-Z0-9' | tail -c 8)
if [ -z "$run_id" ]; then
  echo "remote-harness: the run directory does not provide a usable resource suffix" >&2
  exit 2
fi
vol="runpool-sqlite-state-$run_id"
writer="runpool-sqlite-writer-$run_id"
# Scaffolding image, digest-pinned like every image the project touches.
img='busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662'

cleanup() {
  docker rm -f "$writer" >/dev/null 2>&1 || true
  docker volume rm "$vol" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker volume create "$vol" >/dev/null

# Read from inside a container rather than from the volume's host path.
# That path is root-owned, so a non-root runner cannot stat it, and the
# filesystem a writing process sees is the one this suite is about.
echo "== filesystem under the named volume =="
docker run --rm -v "$vol":/state "$img" df -T /state | tail -1

echo "== full durability suite on the named volume =="
docker run --rm --memory 256m --cpus 1 \
  -v "$vol":/state -v "$dir":/suite:ro --tmpfs /small:rw,size=8m \
  -e RUNPOOL_SQLITE_CONTRACT_DIR=/state -e RUNPOOL_SQLITE_SMALL_DIR=/small \
  "$img" /suite/sqlite-contract.test -test.v -test.count=1 \
  -test.run '^(TestPragmas|TestCrashRecovery|TestContention|TestSingletonLock|TestBackupRestore|TestDiskFull|TestMigrationMechanics)$'

echo "== container-kill rounds =="
for round in 1 2 3; do
  docker run -d --name "$writer" \
    -v "$vol":/state -v "$dir":/suite:ro \
    -e RUNPOOL_SQLITE_WRITER=1 \
    -e RUNPOOL_SQLITE_DB=/state/kill.db -e RUNPOOL_SQLITE_LOG=/state/kill.log \
    "$img" /suite/sqlite-contract.test >/dev/null
  # Wait for evidence of committed work before killing, so every round
  # verifies recovery of a database that provably had transactions in
  # flight; a fixed pause could kill a writer that never got started.
  # The log is cleared at the top of each round, so this waits on this
  # round's work rather than being satisfied by a predecessor's leftovers
  # -- and running out of patience is a failed round, not a quiet kill of
  # a writer that never started.
  ready=0
  for ((attempt = 0; attempt < 150; attempt++)); do
    if docker run --rm -v "$vol":/state "$img" sh -c 'test -s /state/kill.log'; then
      ready=1
      break
    fi
    sleep 0.2
  done
  if [ "$ready" != 1 ]; then
    echo "round $round: the writer produced no committed work to kill" >&2
    docker logs "$writer" >&2 || true
    docker kill -s KILL "$writer" >/dev/null 2>&1 || true
    docker rm "$writer" >/dev/null 2>&1 || true
    exit 1
  fi
  docker kill -s KILL "$writer" >/dev/null
  docker rm "$writer" >/dev/null
  docker run --rm -v "$vol":/state -v "$dir":/suite:ro \
    -e RUNPOOL_SQLITE_CONTRACT_DIR=/state \
    -e RUNPOOL_SQLITE_VERIFY_DB=/state/kill.db -e RUNPOOL_SQLITE_VERIFY_LOG=/state/kill.log \
    "$img" /suite/sqlite-contract.test -test.run 'TestVerifyExisting' -test.v -test.count=1
  echo "round $round: recovered"
  echo "RUNPOOL_CASE container-kill-round-$round passed"
done

echo "SQLite durability suite passed"
