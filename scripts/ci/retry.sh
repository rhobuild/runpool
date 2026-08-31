#!/usr/bin/env bash
set -euo pipefail
# Retry a network command with a bounded, fixed delay. The helper is blind by
# design: provider error text is not a stable interface on which to classify
# transient failures. Every failed attempt is reported and the final command
# status is preserved.
#
# Do not use this for commands that consume stdin, security verdicts, or local
# deterministic checks. A retry cannot replay consumed input and must not turn
# a reproducible failure into repeated noise.
#
# Usage: scripts/ci/retry.sh <command> [args...]
#        RETRY_ATTEMPTS and RETRY_PAUSE override the defaults.
attempts=${RETRY_ATTEMPTS:-3}
pause=${RETRY_PAUSE:-10}

if [ "$#" -eq 0 ]; then
  echo "usage: retry.sh <command> [args...]" >&2
  exit 2
fi

case $attempts in '' | *[!0-9]*)
  echo "retry: RETRY_ATTEMPTS must be a positive integer, not \"$attempts\"" >&2; exit 2 ;;
esac
case $pause in '' | *[!0-9]*)
  echo "retry: RETRY_PAUSE must be a whole number of seconds, not \"$pause\"" >&2; exit 2 ;;
esac
attempts=$((10#$attempts))
pause=$((10#$pause))
if [ "$attempts" -lt 1 ] || [ "$attempts" -gt 10 ]; then
  echo "retry: RETRY_ATTEMPTS must be between 1 and 10, not $attempts" >&2; exit 2
fi
if [ "$pause" -gt 300 ]; then
  echo "retry: RETRY_PAUSE must be at most 300 seconds, not $pause" >&2; exit 2
fi

for ((attempt = 1; attempt <= attempts; attempt++)); do
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
