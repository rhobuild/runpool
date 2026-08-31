#!/usr/bin/env bash
set -euo pipefail

# Enforce the repository-wide unit-test coverage floor. Live contract suites
# cover daemon and provider adapters separately; this floor prevents the
# hermetic surface from regressing unnoticed.
#
# Usage: scripts/verify/coverage.sh [coverage-profile]
if [ "$#" -gt 1 ]; then
  echo "usage: coverage.sh [coverage-profile]" >&2
  exit 2
fi
profile=${1:-coverage.out}
minimum=${RUNPOOL_COVERAGE_MIN-55.0}

if ! awk -v value="$minimum" 'BEGIN {
  valid = value ~ /^[0-9]+([.][0-9]+)?$/ && value + 0 >= 0 && value + 0 <= 100
  exit !valid
}'; then
  echo "RUNPOOL_COVERAGE_MIN must be a number between 0 and 100, not \"$minimum\"" >&2
  exit 2
fi

if [ ! -f "$profile" ]; then
  echo "coverage profile not found: $profile" >&2
  exit 2
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')
if [ -z "$total" ]; then
  echo "coverage profile has no total" >&2
  exit 1
fi

awk -v total="$total" -v minimum="$minimum" 'BEGIN {
  if (total + 0 < minimum + 0) {
    printf "coverage %.1f%% is below the %.1f%% floor\n", total, minimum > "/dev/stderr"
    exit 1
  }
  printf "coverage %.1f%% meets the %.1f%% floor\n", total, minimum
}'
