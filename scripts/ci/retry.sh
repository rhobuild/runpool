#!/usr/bin/env bash
set -euo pipefail
# Run a command that talks to a registry, and try again when it fails.
#
# Four release cycles were lost to registries answering badly and nothing
# asking twice: a digest verification that could not reach Docker Hub, a
# base image fetched through a 502, a module proxy that reset the stream
# mid-download, and a push GHCR accepted layer by layer and then called an
# unknown blob. None was a fault in this repository, and each cost a
# maintainer a manual re-run of a tagged release.
#
# Blind, and bounded because of it. Deciding which failures are worth
# repeating means matching a registry's wording, which changes without
# telling anyone, and a rule that reads output is a rule that stops
# matching quietly. Three attempts of a build that is genuinely broken
# cost minutes; one attempt at a build whose registry blinked costs the
# release.
#
# Every attempt says which one it was, so a real failure is not three
# identical errors an operator has to tell apart.
#
# Usage: scripts/ci/retry.sh <command> [args...]
#        RETRY_ATTEMPTS and RETRY_PAUSE override the defaults.
attempts=${RETRY_ATTEMPTS:-3}
pause=${RETRY_PAUSE:-10}

if [ "$#" -eq 0 ]; then
  echo "usage: retry.sh <command> [args...]" >&2
  exit 2
fi

for attempt in $(seq 1 "$attempts"); do
  # The status comes from the else branch, not from after the if: a
  # conditional whose test failed completes successfully, so $? there is
  # the if's own zero. Read that way this would have exited 0 on giving
  # up, and a build that never worked would have passed.
  if "$@"; then
    [ "$attempt" -gt 1 ] && echo "retry: succeeded on attempt $attempt" >&2
    exit 0
  else
    status=$?
  fi
  if [ "$attempt" -eq "$attempts" ]; then
    echo "retry: gave up after $attempts attempts; the last exited $status: $*" >&2
    exit "$status"
  fi
  echo "retry: attempt $attempt of $attempts exited $status, waiting ${pause}s: $*" >&2
  sleep "$pause"
done
