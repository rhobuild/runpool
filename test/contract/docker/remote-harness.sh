#!/usr/bin/env bash
set -euo pipefail
# Run the Docker client contract suite on the host under test.
#
# The suite runs on the host rather than inside a container because the
# contract under test is the daemon API itself, and the resource-envelope
# check reads cgroup v2 paths. Both gates and the SSH driver run this
# same file, so the pull fixture is cleared one way rather than three.
#
# RUNPOOL_CONTRACT_QUALIFY travels from the caller: in qualification mode
# the suite additionally requires the exact platform the reviewed lock
# names. No expected value travels with it — the suite embeds the
# reference and compares the host against it.
#
# Usage: test/contract/docker/remote-harness.sh <dir with docker-contract.test>
dir=${1:?usage: remote-harness.sh <dir with docker-contract.test>}

# The image TestPullOnMissingImage pulls. Held equal to the suite's own
# constant by test/consistency: removing something else would leave the
# fixture cached and the pull path unexercised.
fixture='busybox:1.36.1-uclibc@sha256:0872fb3a7632ba9d0ae46a8e832a62b30ce83a6f220b8bb52903d9cf477dabe3'

# The pull-on-missing path only exists while the image is missing: after
# one run the daemon has it cached and the test passes without pulling
# anything. Prove the removal — one that silently failed turns that test
# into a no-op, and a no-op reports as a pass.
docker image rm -f "$fixture" >/dev/null 2>&1 || true
if docker image inspect "$fixture" >/dev/null 2>&1; then
  echo 'the pull fixture is still present; the pull test would exercise nothing' >&2
  exit 1
fi

RUNPOOL_DOCKER_CONTRACT=1 "$dir/docker-contract.test" -test.v -test.count=1
