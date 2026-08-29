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
echo "RUNPOOL_CASE install passed"

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
echo "RUNPOOL_CASE backup passed"

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
echo "RUNPOOL_CASE restore passed"

echo "== drill: upgrade (migration) and integrity =="
# The same binary on both ends proves the state survived a round trip; it
# cannot prove a migration, because there is nothing between the two
# versions to migrate. So the old end is a database the released schema
# wrote, carried in the tree, and the new end is the image built here.
docker run --rm -v "$vol":/state -v "$dir/drill-seed":/drill-seed:ro "$bb" /drill-seed verify /state >/dev/null
echo "upgrade: current schema opens the restored state and the marker survives"

released_db=internal/store/testdata/v1.0.0.db
if [ ! -f "$released_db" ]; then
  echo "upgrade drill: $released_db is missing; the old end of this comparison is gone"; exit 1
fi
old_vol="$vol-released"
docker volume create "$old_vol" >/dev/null
trap 'docker volume rm "$old_vol" >/dev/null 2>&1 || true; cleanup' EXIT
docker run --rm -v "$old_vol":/state -v "$PWD/$released_db":/seed.db:ro "$bb" \
  cp /seed.db /state/runpool.db
before=$(docker run --rm -v "$old_vol":/state:ro "$bb" ls /state | tr '\n' ' ')

# Opening it read-write is the migration, and only the helper does that:
# `status` is a reporting command, opens read-only, and refuses a
# database that predates the build rather than moving it -- which is what
# it says when asked, and why it cannot stand in here.
if ! version=$(docker run --rm -v "$old_vol":/state \
    -v "$dir/drill-seed":/drill-seed:ro "$bb" /drill-seed open /state 2>&1); then
  echo "upgrade drill: the current build refused a database the released schema wrote:"
  echo "$version"; exit 1
fi
echo "upgrade: the released database opened at schema $version"

after=$(docker run --rm -v "$old_vol":/state:ro "$bb" ls /state | tr '\n' ' ')
echo "upgrade: released state before [$before] after [$after]"

# A schema that moved leaves a copy to roll back to, and the release it
# came from must then refuse the migrated database rather than read it
# under a vocabulary it does not have. Both are conditional on a
# migration having happened, which is what arms them when 000002 lands.
if printf '%s' "$after" | grep -q 'pre-migration'; then
  # At the path that image looks in, not at ours: mounted anywhere else
  # it reports an instance that has not run and exits zero, which reads
  # as a refusal and is not one.
  released_out=$(docker run --rm -v "$old_vol":/var/lib/runpool/state \
    ghcr.io/rhobuild/runpool:v1.0.0 status 2>&1) && {
    echo "upgrade drill: the released build read a database this one migrated:"
    printf '%s\n' "$released_out" | head -3; exit 1
  }
  case "$released_out" in
    *"schema"*) ;;
    *) echo "upgrade drill: v1.0.0 failed for some other reason:"
       printf '%s\n' "$released_out" | head -3; exit 1 ;;
  esac
  echo "upgrade: the schema moved, a pre-migration copy exists, and v1.0.0 refuses the result"
else
  echo "upgrade: no migration pending for the released schema"
fi
echo "RUNPOOL_CASE upgrade passed"

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
echo "RUNPOOL_CASE uninstall passed"

echo
echo "lifecycle drills passed: install, backup, restore, upgrade, uninstall"
