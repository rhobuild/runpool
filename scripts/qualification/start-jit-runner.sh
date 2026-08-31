#!/usr/bin/env bash
set -euo pipefail
# Start one just-in-time runner for the live GitHub Actions contracts.
#
# RUNPOOL_CONTRACT_RUNNER_CMD points at this script. The suite appends the
# runner name as the last argument and writes the JIT bundle to stdin, then
# waits for the command to finish — so this starts the runner and returns
# rather than following it.
#
# The bundle never reaches the host. It is delivered over stdin into an
# already-running container, written there with a private mode, and removed
# before the runner is executed; the only place it becomes an argument is the
# upstream runner's required --jitconfig, inside the disposable container.
# That is the same boundary the capsule supervisor keeps, for the same reason:
# a bundle in host state or in a log outlives the runner it authorises.
#
# The runner image is the digest pinned in build/images.lock.json, so the
# contracts exercise the bytes the capsule is built from.
#
# Usage: start-jit-runner.sh <runner-name>   # JIT bundle on stdin
cd "$(dirname "$0")/../.."

if [ "$#" -ne 1 ]; then
  echo "usage: start-jit-runner.sh <runner-name>" >&2
  exit 2
fi
name=${1:?usage: start-jit-runner.sh <runner-name>}
case $name in
  [[:alnum:]]*) ;;
  *) echo "start-jit-runner: runner name must begin with an alphanumeric character" >&2; exit 2 ;;
esac
case $name in
  *[![:alnum:]_.-]*)
    echo "start-jit-runner: runner name contains characters Docker cannot use in a container name" >&2
    exit 2
    ;;
esac
container="runpool-contract-${name}"

image=$(go run ./internal/qualification/cmd/image-lock -ref runner)

# The container waits for the bundle instead of receiving it as an argument or
# an environment value, both of which would be readable from the host for as
# long as the container lived.
docker run --detach --rm --name "$container" --entrypoint sh "$image" -c '
  while [ ! -s /tmp/jitconfig ]; do sleep 0.1; done
  config=$(cat /tmp/jitconfig)
  rm -f /tmp/jitconfig
  exec ./run.sh --jitconfig "$config"
' >/dev/null

# A container waiting for a bundle that never arrives waits forever. Keep
# cleanup armed across errors and signals until the complete bundle is visible
# inside the container.
cleanup_required=1
cleanup() {
  if [ "$cleanup_required" -eq 1 ]; then
    docker rm --force -- "$container" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Delivered under a temporary name and renamed, so the waiting loop never
# observes a partly written bundle and starts a runner with a truncated one.
# Nothing here echoes it: the suite fails the contract if it finds the bundle
# in this command's output, and it is right to.
docker exec -i "$container" sh -c \
  'umask 077; cat > /tmp/jitconfig.part && mv /tmp/jitconfig.part /tmp/jitconfig'

cleanup_required=0
printf 'started %s\n' "$container"
