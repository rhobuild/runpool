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
# Three things this does not cover, and they are not oversights.
#
# A step that is a `uses:` action rather than a shell command -- the SBOM
# scan, the four attestations that push to a registry -- is not a command
# line to wrap, and reaching them would mean reimplementing each action.
#
# `docker login` is fed its token on stdin by a pipe, which the first
# attempt consumes: wrapped as written, every retry would authenticate
# against an empty stdin. A login that fails is also far more often a
# permission than a blink.
#
# `go tool govulncheck` fetches the vulnerability index from vuln.go.dev
# on every run. That is not a module fetch and no warm cache prevents it,
# so the download below does not cover it -- but wrapping it would repeat
# a security gate, printing two spurious attempts over a real finding and
# spending the pause on a verdict that will not change. It is named here
# because leaving it unnamed would let this file read as covering every
# reach on the release path, which it does not.
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

# `seq 1 0` prints nothing, and so does `seq 1 nonsense`: the loop below
# would not run once, the script would fall off the end, and the step
# would report success for a command that was never executed. This is the
# third door that failure has come through -- first `$?` after a failed
# `if`, then a literal `0`, then `00`, which is all digits and is not the
# string "0" -- so the check is on the value now, not on how it is
# spelled. `10#` forces base ten: without it a leading zero is octal, and
# `08` is not a number at all.
case $attempts in '' | *[!0-9]*)
  echo "retry: RETRY_ATTEMPTS must be a positive integer, not \"$attempts\"" >&2; exit 2 ;;
esac
case $pause in '' | *[!0-9]*)
  echo "retry: RETRY_PAUSE must be a whole number of seconds, not \"$pause\"" >&2; exit 2 ;;
esac
attempts=$((10#$attempts))
pause=$((10#$pause))
# An upper bound because `seq` materializes the whole list before the
# loop body runs even once: an absurd value does not retry a great many
# times, it hangs having run the command zero times, until the job's own
# timeout kills it. Ten is far above anything a registry blink needs.
if [ "$attempts" -lt 1 ] || [ "$attempts" -gt 10 ]; then
  echo "retry: RETRY_ATTEMPTS must be between 1 and 10, not $attempts" >&2; exit 2
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
