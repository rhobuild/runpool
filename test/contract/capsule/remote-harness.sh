#!/usr/bin/env bash
set -euo pipefail
# Run the outer-capsule, egress and budget contract suite on the host
# under test.
#
# The capsule image is the caller's to prepare, always tagged
# runpool-capsule:dev: the pull-request gate builds it from the pinned
# inputs, and the release gate tags the exact candidate it is qualifying.
# What the caller does not choose is the deadline, which lives here so
# both gates allow the suite the same time to finish.
#
# The lifecycle drives a real runner with a fake credential -- the runner
# rejects it and exits, which proves it ran without needing a provider.
# The bypass suite proves the capsule has no route out and that its only
# egress is the policy.
#
# Usage: test/contract/capsule/remote-harness.sh <dir with capsule-contract.test>
dir=${1:?usage: remote-harness.sh <dir with capsule-contract.test>}

RUNPOOL_CAPSULE_CONTRACT=1 "$dir/capsule-contract.test" -test.v -test.count=1 -test.timeout=30m
