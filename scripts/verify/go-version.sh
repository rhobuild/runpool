#!/usr/bin/env bash
set -euo pipefail
# The builder images must name the Go that go.mod declares. Nothing else
# ties them: an image tag and a module directive are separate ecosystems to
# every updater, so they drift silently and the drift is invisible in a
# green build. The binary an image ships would then be compiled by a
# toolchain no test ever ran, which is the one thing a reproducible build
# is supposed to rule out.
#
# Dependabot is configured to propose only patch bumps for the builder, so
# this is the check behind that policy rather than a substitute for it.
#
# Usage: scripts/verify/go-version.sh
cd "$(dirname "$0")/../.."
status=0

want=$(awk '/^go [0-9]/ {print $2; exit}' go.mod)
if [ -z "$want" ]; then
  echo "FAIL  go.mod declares no Go version"
  exit 1
fi

for dockerfile in build/*/Dockerfile; do
  # Taken without a pipe: head would be free to close on sed mid-write.
  builders=$(sed -n 's/^FROM golang:\([0-9][^-@ ]*\).*/\1/p' "$dockerfile")
  got=${builders%%$'\n'*}
  if [ -z "$got" ]; then
    echo "FAIL  $dockerfile: no golang builder stage found"
    status=1
  elif [ "$got" = "$want" ]; then
    echo "ok    $dockerfile golang:$got"
  else
    echo "FAIL  $dockerfile builds with golang:$got, go.mod declares $want"
    status=1
  fi
done

exit $status
