#!/usr/bin/env bash
# Checks Go statement coverage per package against the floors in
# docs/spec/13-testing.md: 75% for every package, 85% for internal/auth,
# internal/repo, internal/gitserver and internal/hfapi. Generated code and
# packages tested only by the compat suites are excluded.
# go test runs through scripts/go-test.sh, which runs a package again if it
# failed to build only because GOCACHE files were deleted mid-run.
# Usage: scripts/coverage.sh [extra go test flags]
set -euo pipefail
cd "$(dirname "$0")/.."
exclude='^github.com/jamesagarside/hangar/(internal/api/gen|compat/|internal/pins)'
strict='^github.com/jamesagarside/hangar/internal/(auth|repo|gitserver|hfapi)$'
out="$(scripts/go-test.sh go test -count=1 -cover "$@" ./... 2>&1)" || { echo "$out"; exit 1; }
fail=0
while read -r pkg pct; do
  [[ "$pkg" =~ $exclude ]] && continue
  floor=75
  [[ "$pkg" =~ $strict ]] && floor=85
  if awk -v p="$pct" -v f="$floor" 'BEGIN { exit !(p < f) }'; then
    echo "FAIL $pkg ${pct}% < ${floor}%"
    fail=1
  else
    echo "ok   $pkg ${pct}%"
  fi
done < <(echo "$out" | grep -oE 'github.com/jamesagarside/hangar/[^[:space:]]+[[:space:]].*coverage: [0-9.]+%' \
  | sed -E 's/^([^[:space:]]+).*coverage: ([0-9.]+)%.*/\1 \2/')
exit $fail
