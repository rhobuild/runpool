#!/usr/bin/env bash
set -euo pipefail
# The builder images and the workflows must name the Go that go.mod
# declares. Nothing else ties them: an image tag, a workflow variable and a
# module directive are separate ecosystems to every updater, so they drift
# silently and the drift is invisible in a green build. The binary an image
# ships would then be compiled by a toolchain no test ever ran, which is the
# one thing a reproducible build is supposed to rule out.
#
# The workflow half is the same argument one step earlier: a gate that
# resolves its own compiler proves something about a toolchain the release
# may never use. Every workflow here runs Go, so every workflow states it.
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

for workflow in .github/workflows/*.yml; do
  # Every pin in the file, not the first: a workflow may state one and a
  # job below it another, and the one that disagrees is the drift this
  # exists to find. A trailing comment is tolerated so a pin that is
  # there does not report as absent.
  pinned=$(sed -n 's/^ *GOTOOLCHAIN: *go\([0-9][^ #]*\).*$/\1/p' "$workflow")
  if [ -z "$pinned" ]; then
    echo "FAIL  $workflow pins no GOTOOLCHAIN, so its steps may resolve another compiler"
    status=1
    continue
  fi
  for got in $pinned; do
    if [ "$got" = "$want" ]; then
      echo "ok    $workflow GOTOOLCHAIN go$got"
    else
      echo "FAIL  $workflow pins GOTOOLCHAIN go$got, go.mod declares $want"
      status=1
    fi
  done
done

exit $status
