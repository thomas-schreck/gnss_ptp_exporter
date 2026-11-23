#!/usr/bin/env bash
# Run every Go fuzz target for the given duration (default 30s).
set -euo pipefail
fuzztime="${1:-30s}"
cd "$(dirname "$0")/.."
for pkg in $(go list ./...); do
  for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
    echo "== $pkg $target ($fuzztime)"
    go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" "$pkg"
  done
done
