#!/usr/bin/env bash
set -euo pipefail
# The controller's whole shutdown — the drain window plus the shared
# budget for closing every message session — must fit inside the
# deployment's stop grace period. Nothing else ties them: two Go
# constants and a Compose key are separate ecosystems, and the drift is
# invisible in a green build because neither side can see the other.
#
# The drift is not cosmetic. A capsule running a job outlives any drain
# window, so the window is always spent whenever work is in flight. When
# shutdown outlasts the grace period the platform sends SIGKILL first,
# the deferred session closes never run, and every restart with a live
# job leaves the broker holding a session the next start has to wait out
# as a conflict.
#
# Usage: scripts/verify/drain-window.sh
cd "$(dirname "$0")/../.."
status=0

seconds_from_go() {
  # Exactly "N * time.Second" or "N * time.Minute". Anything else is
  # unreadable rather than zero: a form this cannot parse must fail the
  # check, not pass it with a window of no seconds.
  awk '{
    if ($0 !~ /^[0-9]+ \* time\.(Second|Minute)$/) { print ""; exit }
    if ($0 ~ /time\.Minute/) { print $1 * 60 } else { print $1 }
  }' <<<"$1"
}

seconds_from_compose() {
  # "2m", "90s", "1m30s" is not emitted by Compose docs but is accepted
  case "$1" in
    *m) echo $(( ${1%m} * 60 )) ;;
    *s) echo "${1%s}" ;;
    *) echo "" ;;
  esac
}

drain_expr=$(sed -n 's/^[[:space:]]*drainTimeout[[:space:]]*=[[:space:]]*\(.*\)$/\1/p' internal/app/serve.go)
if [ -z "$drain_expr" ]; then
  echo "FAIL  internal/app/serve.go declares no drainTimeout"
  exit 1
fi
drain=$(seconds_from_go "$drain_expr")
if [ -z "$drain" ]; then
  echo "FAIL  internal/app/serve.go: cannot read drainTimeout from '$drain_expr'"
  exit 1
fi

close_expr=$(sed -n 's/^[[:space:]]*sessionCloseBudget[[:space:]]*=[[:space:]]*\(.*\)$/\1/p' internal/app/serve.go)
if [ -z "$close_expr" ]; then
  echo "FAIL  internal/app/serve.go declares no sessionCloseBudget"
  exit 1
fi
close=$(seconds_from_go "$close_expr")
if [ -z "$close" ]; then
  echo "FAIL  internal/app/serve.go: cannot read sessionCloseBudget from '$close_expr'"
  exit 1
fi
shutdown=$(( drain + close ))

compose=deploy/compose/compose.yaml
grace_raw=$(sed -n 's/^[[:space:]]*stop_grace_period:[[:space:]]*\([^[:space:]]*\).*/\1/p' "$compose")
if [ -z "$grace_raw" ]; then
  echo "FAIL  $compose declares no stop_grace_period"
  exit 1
fi
grace=$(seconds_from_compose "$grace_raw")
if [ -z "$grace" ]; then
  echo "FAIL  $compose: cannot read stop_grace_period from '$grace_raw'"
  status=1
elif [ "$grace" -gt "$shutdown" ]; then
  echo "ok    $compose stop_grace_period ${grace}s over a ${shutdown}s shutdown (${drain}s drain + ${close}s session close)"
else
  echo "FAIL  $compose stop_grace_period is ${grace}s and shutdown needs ${shutdown}s (${drain}s drain + ${close}s session close);"
  echo "      the platform kills the controller before it closes its sessions"
  status=1
fi

exit $status
