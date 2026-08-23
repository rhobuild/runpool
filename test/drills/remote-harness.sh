#!/usr/bin/env bash
set -euo pipefail
# Remote half of the lifecycle drills. Runs on the Docker host: builds
# the controller image from the shipped tree, then walks install →
# backup → wipe → restore → upgrade check → uninstall, asserting each
# step. Everything it creates is named after this run and removed at
# the end, success or failure.
#
# Usage: remote-harness.sh <dir>
dir=${1:?usage: remote-harness.sh <dir with src.tgz and drill-seed>}
run_id=$(basename "$dir" | tr -dc 'a-zA-Z0-9' | tail -c 8)
vol="runpool-drill-state-$run_id"
img="runpool:drill-$run_id"
# Busybox for volume plumbing, digest-pinned like everything else.
bb='busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662'

cleanup() {
  docker volume rm "$vol" >/dev/null 2>&1 || true
  docker rmi "$img" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$dir/src"
tar -xzf "$dir/src.tgz" -C "$dir/src" 2>/dev/null
cd "$dir/src"

echo "== drill: install =="
docker build -q -f build/controller/Dockerfile -t "$img" . >/dev/null
docker volume create "$vol" >/dev/null
# The image runs and states its version. Doctor on an unconfigured
# host must do two things at once: pass every host check, and still
# exit non-zero naming the missing configuration — an all-green exit
# with nothing configured would be a lie about readiness.
docker run --rm "$img" version
doctor_out=$(docker run --rm -v /var/run/docker.sock:/var/run/docker.sock:ro "$img" doctor 2>&1) \
  && { echo "install drill: doctor exited zero without a configuration"; exit 1; }
echo "$doctor_out"
if echo "$doctor_out" | grep -q '^fail'; then
  echo "install drill: a host check failed on the reference host"; exit 1
fi
echo "$doctor_out" | grep -q 'RUNPOOL_GITHUB_URL' || {
  echo "install drill: doctor did not name the missing configuration"; exit 1; }
# Fail-closed proof: serve without a credential must refuse, not start.
if docker run --rm -v "$vol":/var/lib/runpool/state "$img" serve >/dev/null 2>&1; then
  echo "install drill: serve started without a credential"; exit 1
fi
echo "install: image runs, doctor passes, serve refuses without a credential"

echo "== drill: seed state (stands in for a served instance) =="
# The helper runs inside a container so the files carry the same
# ownership the controller would give them.
docker run --rm -v "$vol":/state -v "$dir/drill-seed":/drill-seed:ro "$bb" \
  /drill-seed create /state > "$dir/instance-id"
instance=$(cat "$dir/instance-id")
echo "seeded instance $instance"

echo "== drill: backup =="
# The runbook's procedure verbatim: archive the state volume while no
# controller runs. The singleton lock makes 'no controller' checkable.
docker run --rm -v "$vol":/state:ro "$bb" tar -czf - -C /state . > "$dir/state-backup.tgz"
[ -s "$dir/state-backup.tgz" ] || { echo "backup drill: empty archive"; exit 1; }
# Non-empty is not the property that matters. A volume name that does not
# exist is created on the spot by `docker run`, and an archive of that
# empty directory is valid, non-empty and useless — silent until a
# restore needs it. What has to be in there is the database.
if ! tar -tzf "$dir/state-backup.tgz" | grep -q 'runpool\.db$'; then
  echo "backup drill: the archive holds no state database:"
  tar -tzf "$dir/state-backup.tgz" | sed 's/^/  /'
  exit 1
fi
echo "backup: $(wc -c < "$dir/state-backup.tgz") bytes, database present"

echo "== drill: disaster (wipe) and restore =="
docker volume rm "$vol" >/dev/null
docker volume create "$vol" >/dev/null
docker run --rm -v "$vol":/state -i "$bb" tar -xzf - -C /state < "$dir/state-backup.tgz"
restored=$(docker run --rm -v "$vol":/state -v "$dir/drill-seed":/drill-seed:ro "$bb" /drill-seed verify /state)
if [ "$restored" != "$instance" ]; then
  echo "restore drill: instance $restored after restore; want $instance"; exit 1
fi
# The CLI agrees: status reads the restored books. Captured before it is
# trimmed: piping the command straight into head lets head close the pipe
# first on a long report, which kills the writer with EPIPE and, under
# pipefail, fails the drill for finishing its own output.
status_report=$(docker run --rm -v "$vol":/var/lib/runpool/state -v /var/run/docker.sock:/var/run/docker.sock:ro \
  "$img" status)
printf '%s\n' "$status_report" | head -3
echo "restore: instance identity and audit marker survived"

echo "== drill: upgrade (migration) and integrity =="
# Pre-release, 'upgrade' is: the new binary opens existing state and
# migrates forward; the seed helper (built from this same tree) does
# exactly that on open, and verify proves the data survived migration.
# Rollback for a failed upgrade is the restore above — down-migrations
# exist, but restoring the pre-upgrade backup is the documented path.
docker run --rm -v "$vol":/state -v "$dir/drill-seed":/drill-seed:ro "$bb" /drill-seed verify /state >/dev/null
echo "upgrade: current schema opens the restored state and the marker survives"

echo "== drill: uninstall =="
# Uninstall must remove everything the instance owns — including a
# labeled resource created out-of-band — and must refuse without the
# instance id as confirmation.
docker volume create --label io.runpool.managed=true --label "io.runpool.instance=$instance" \
  --label io.runpool.role=cache-lane "runpool-cache-drill$run_id" >/dev/null
if docker run --rm -v "$vol":/var/lib/runpool/state -v /var/run/docker.sock:/var/run/docker.sock:ro \
  "$img" uninstall --confirm=wrong-id >/dev/null 2>&1; then
  echo "uninstall drill: accepted a wrong confirmation id"; exit 1
fi
docker run --rm -v "$vol":/var/lib/runpool/state -v /var/run/docker.sock:/var/run/docker.sock:ro \
  "$img" uninstall "--confirm=$instance"
left=$(docker volume ls -q --filter "label=io.runpool.instance=$instance")
if [ -n "$left" ]; then
  echo "uninstall drill: owned volumes left behind: $left"; exit 1
fi
echo "uninstall: refused the wrong id, removed the owned volume"

echo
echo "lifecycle drills passed: install, backup, restore, upgrade, uninstall"
