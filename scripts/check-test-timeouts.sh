#!/usr/bin/env bash
# Checks that every `go test` run of the compat suites (the compat build tag)
# in the Makefile and the workflows passes -timeout. The suites drive real
# git, git-lfs and Python clients, and on a busy host one package can take
# longer than go test's default of 10 minutes even though every test passes.
# Usage: scripts/check-compat-timeout.sh [files...]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then
  set -- Makefile .github/workflows/*.yml
fi

failed=0
checked=0
for f in "$@"; do
  # Join backslash continuations so a command split over lines is one line.
  while IFS= read -r line; do
    if ! printf '%s\n' "$line" | grep -Eq '(^|[^A-Za-z0-9_])(go|\$\(GO\)) test .*-tags[ =]['"'"'"]?[A-Za-z0-9_, ]*compat'; then
      continue
    fi
    checked=$((checked + 1))
    if ! printf '%s\n' "$line" | grep -Eq '(^|[[:space:]])-timeout[ =]'; then
      echo "error: ${f}: a compat go test run has no -timeout: $(printf '%s' "$line" | sed 's/^[[:space:]]*//')" >&2
      failed=1
    fi
  done < <(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$f")
done

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "All ${checked} compat go test runs pass -timeout."
