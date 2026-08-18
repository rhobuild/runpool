#!/usr/bin/env bash
set -euo pipefail
# The capsule's control surface is a contract between two programs that
# cannot import each other: the supervisor is the capsule image's PID 1
# and depends on nothing, and the controller drives it over exec. Every
# value they must agree on is therefore declared twice, and a comment on
# each side names its counterpart. This is that comment made executable.
#
# Drift here is silent in a green build and expensive in production: a
# protocol version that disagrees refuses every capsule, and an abort
# exit code that disagrees reads a job that never started as one that ran.
#
# Usage: scripts/verify/capsule-protocol.sh
cd "$(dirname "$0")/../.."
status=0

supervisor=cmd/capsule-supervisor/main.go

check() {
  local what=$1 supervisor_expr=$2 launcher=$3 launcher_expr=$4
  local mine theirs
  # Either form a Go declaration takes: inside a const block, or a
  # single `const NAME = value` of its own.
  local pattern='s/^[[:space:]]*\(const[[:space:]]\{1,\}\)\{0,1\}NAME[[:space:]]*=[[:space:]]*\([^[:space:]]*\).*/\2/p'
  mine=$(sed -n "${pattern//NAME/$supervisor_expr}" "$supervisor")
  theirs=$(sed -n "${pattern//NAME/$launcher_expr}" "$launcher")
  if [ -z "$mine" ]; then
    echo "FAIL  $supervisor declares no $supervisor_expr"
    status=1
  elif [ -z "$theirs" ]; then
    echo "FAIL  $launcher declares no $launcher_expr"
    status=1
  elif [ "$mine" = "$theirs" ]; then
    echo "ok    $what agrees on both sides ($mine)"
  else
    echo "FAIL  $what disagrees: $supervisor_expr is $mine, $launcher_expr is $theirs"
    status=1
  fi
}

check "the control protocol version" protocolVersion \
  internal/capsule/capsule.go ProtocolVersion
check "the aborted exit code" exitAborted \
  internal/capsule/observation.go SupervisorAbortedExitCode

exit $status
