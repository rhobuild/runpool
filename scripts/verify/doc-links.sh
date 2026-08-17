#!/usr/bin/env bash
set -euo pipefail
# Check that every relative link and image path in the Markdown docs
# resolves to a file that exists. It is deliberately offline: external
# URLs are not fetched — that would make the gate flaky and slow —
# only the in-repo links a documentation reorganization can silently
# break. A link with a #fragment is checked up to the fragment; a bare
# #fragment is a same-file anchor and is skipped.
#
# Usage: scripts/verify/doc-links.sh
cd "$(dirname "$0")/../.."

broken=$(mktemp)
trap 'rm -f "$broken"' EXIT

git ls-files -co --exclude-standard -- '*.md' | sort -u | while IFS= read -r file; do
  [ -f "$file" ] || continue
  dir=$(dirname "$file")
  # A document with no links makes grep exit 1, which under `set -e`
  # would end the run rather than report a clean file.
  { grep -oE '\]\([^)]+\)' "$file" || true; } | sed -E 's/^\]\(//; s/\)$//' | while IFS= read -r target; do
    path=${target%%#*}   # drop a trailing anchor
    path=${path%% *}     # drop a link title
    case "$path" in
      http://* | https://* | mailto:* | "") continue ;;
    esac
    [ -e "$dir/$path" ] || echo "BROKEN  $file -> $target" >> "$broken"
  done
done

if [ -s "$broken" ]; then
  cat "$broken"
  echo
  echo "$(wc -l < "$broken" | tr -d ' ') broken documentation link(s)"
  exit 1
fi
echo "all relative documentation links resolve"
