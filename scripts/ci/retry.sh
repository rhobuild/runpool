#!/usr/bin/env bash
set -euo pipefail
# Run a command that fetches from a network service, and try again when
# it fails. Registries and the Go module proxy, which is what the release
# path reaches.
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
# Two things on the release path this cannot cover, and they are not
# oversights. A step that is a `uses:` action rather than a shell command
# -- the SBOM scan, the four attestations that push to a registry -- is
# not a command line to wrap, and reaching them would mean reimplementing
# each action. And `docker login` is fed its token on stdin by a pipe,
# which the first attempt consumes: wrapped as written, every retry would
# authenticate against nothing. A login that fails is also far more often
# a permission than a blink.
#
# What gets wrapped is the fetch, not the work that follows it. Every job
# that sets up Go downloads its modules through this, and then builds,
# tests and verifies from a warm cache. Wrapping the go steps themselves
# would repeat gates whose failure means the release is wrong -- printing
# two spurious attempts over the most important error in the system, and
# spending the pause on a verdict that will not change.
#
# Usage: scripts/ci/retry.sh <command> [args...]
#        RETRY_ATTEMPTS and RETRY_PAUSE override the defaults.
attempts=${RETRY_ATTEMPTS:-3}
pause=${RETRY_PAUSE:-10}

if [ "$#" -eq 0 ]; then
  echo "usage: retry.sh <command> [args...]" >&2
  exit 2
fi

# `seq 1 0` prints nothing, and so does `seq 1 nonsense`. The loop below
# would not run once, the script would fall off the end, and the step
# would report success for a command that was never executed -- the same
# failure as exiting 0 on giving up, arriving by a different door.
case $attempts in
  '' | *[!0-9]* | 0) echo "retry: RETRY_ATTEMPTS must be a positive integer, not \"$attempts\"" >&2; exit 2 ;;
esac
case $pause in
  '' | *[!0-9]*) echo "retry: RETRY_PAUSE must be a whole number of seconds, not \"$pause\"" >&2; exit 2 ;;
esac

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
