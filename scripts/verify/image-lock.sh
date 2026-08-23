#!/usr/bin/env bash
set -euo pipefail
# Re-resolve every tag in the image lock against its registry and fail if
# a digest has drifted, and check that every platform the lock says a
# release builds for is a platform the image actually publishes. The lock
# is what the controller executes, so drift means the privileged payload
# changed under a name already reviewed — and a digest bump that quietly
# drops a platform leaves the declared list and the buildable list
# agreeing with each other while agreeing with nothing upstream. This is
# the one check that looks at the registry rather than at another list.
#
# Needs network and `docker buildx`; the hermetic half — the embedded
# copy matching the reviewed copy, and the declared list matching what
# the code will build — is a Go test (internal/app/images_lock_test.go)
# and runs with the ordinary suite.
#
# Usage: scripts/verify/image-lock.sh
cd "$(dirname "$0")/../.."
lock=build/images.lock.json
status=0

names=$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["images"]))' "$lock")
declared=$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["platforms"]))' "$lock")
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
    continue
  fi

  # The digest is right; now the platforms behind it. By digest rather
  # than by tag, so these are the bytes just verified and not whatever
  # the tag resolves to a moment later. Attestation manifests carry no
  # architecture and are skipped rather than reported as a platform
  # nobody asked for.
  if ! published=$(docker buildx imagetools inspect "${ref%:*}@${want}" --format '{{json .Manifest}}' |
    python3 -c '
import json, sys
m = json.load(sys.stdin)
out = []
for d in m.get("manifests", []):
    p = d.get("platform") or {}
    if p.get("architecture") in (None, "unknown"):
        continue
    out.append(p["os"] + "/" + p["architecture"])
print(" ".join(sorted(set(out))))'); then
    echo "FAIL  $name ($ref): the published platforms could not be read"
    status=1
    continue
  fi
  for platform in $declared; do
    case " $published " in
      *" $platform "*) ;;
      *)
        echo "FAIL  $name $ref: the lock builds for $platform and the image publishes $published"
        status=1
        ;;
    esac
  done
done
exit $status
