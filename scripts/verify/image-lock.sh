#!/usr/bin/env bash
set -euo pipefail
# Re-resolve every tag in the image lock against its registry and fail if
# a digest has drifted. The lock is what the controller executes, so
# drift means the privileged payload changed under a name already
# reviewed. Needs network and `docker buildx`; the hermetic half — the
# embedded copy matching the reviewed copy — is a Go test
# (internal/app/images_lock_test.go) and runs with the ordinary suite.
#
# Usage: scripts/verify/image-lock.sh
cd "$(dirname "$0")/../.."
lock=build/images.lock.json
status=0

names=$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["images"]))' "$lock")
for name in $names; do
  ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["images"][sys.argv[2]]["ref"])' "$lock" "$name")
  want=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["images"][sys.argv[2]]["digest"])' "$lock" "$name")
  if ! got=$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"'); then
    echo "FAIL  $name ($ref): registry unreachable, so the digest could not be verified"
    status=1
    continue
  fi
  if [ "$got" = "$want" ]; then
    echo "ok    $name $ref"
  else
    echo "FAIL  $name $ref: lock says $want, registry says $got"
    status=1
  fi
done
exit $status
