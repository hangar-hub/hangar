#!/usr/bin/env bash
# Checks that the long `go test` runs in the Makefile and the workflows pass
# -timeout: every run of the compat suites (the compat build tag), every run
# of the unit and integration tests (the integration build tag) and every
# scripts/coverage.sh run, which passes its flags to go test. On a busy host
# one package can take longer than go test's default of 10 minutes even
# though every test passes: the compat suites drive real git, git-lfs and
# Python clients, and internal/datasets/pipeline, internal/upstream and other
# git-heavy packages ran about 20 times slower on a loaded host.
# Usage: scripts/check-test-timeouts.sh [files...]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then
  set -- Makefile .github/workflows/*.yml
fi

tagged='(^|[^A-Za-z0-9_])(go|\$\(GO\)) test .*-tags[ =]['"'"'"]?[A-Za-z0-9_, ]*(compat|integration)'
coverage='(^|[^A-Za-z0-9_./-])(\./)?scripts/coverage\.sh([[:space:]]|$)'

failed=0
checked=0
for f in "$@"; do
  # Join backslash continuations so a command split over lines is one line.
  while IFS= read -r line; do
    if ! printf '%s\n' "$line" | grep -Eq "$tagged|$coverage"; then
      continue
    fi
    checked=$((checked + 1))
    if ! printf '%s\n' "$line" | grep -Eq '(^|[[:space:]])-timeout[ =]'; then
      echo "error: ${f}: a long go test run has no -timeout: $(printf '%s' "$line" | sed 's/^[[:space:]]*//')" >&2
      failed=1
    fi
  done < <(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$f")
done

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "All ${checked} long go test runs pass -timeout."
