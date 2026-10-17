#!/bin/sh
# Regenerates code in a temporary copy and fails if it differs from the working tree.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Copy sources without generated output so stale files cannot hide drift
tar -C "$root" --exclude=.git --exclude=gen --exclude=internal/graph/generated --exclude=internal/graph/model --exclude=bin --exclude=tmp -cf - . | tar -C "$tmp" -xf -
(cd "$tmp" && make generate >/dev/null)

status=0
for dir in gen internal/graph/generated internal/graph/model; do
  if ! diff -r "$root/$dir" "$tmp/$dir" >/dev/null; then
    echo "generate-check: $dir is out of date; run make generate and commit" >&2
    status=1
  fi
done
[ "$status" -eq 0 ] && echo "generate-check: generated code is up to date"
exit "$status"
