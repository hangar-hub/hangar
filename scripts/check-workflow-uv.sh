#!/usr/bin/env bash
# Checks that every workflow job that needs uv installs it with
# astral-sh/setup-uv. A job needs uv when a step runs uv itself, runs
# `make docs` or `make test-compat`, or runs Go tests with the compat build
# tag: the compat Go suites call `uv run --project compat` for the hf CLI and
# the Python clients, and fail without it. Needs yq (mikefarah, v4).
# Usage: scripts/check-workflow-uv.sh [workflow files...]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then
  set -- .github/workflows/*.yml
fi

# Prints "<job>\t<setup-uv steps>\t<steps that need uv>" for every job.
query='.jobs | to_entries | .[] | .key + "\t"
  + ([.value.steps[] | select((.uses // "") | test("^astral-sh/setup-uv@"))] | length | tostring) + "\t"
  + ([.value.steps[] | select((.run // "") | test("(^|[^A-Za-z0-9_./-])uv |make (docs|test-compat)|-tags[ =]\S*compat"))] | length | tostring)'

failed=0
checked=0
for f in "$@"; do
  while IFS=$'\t' read -r job setup needs; do
    if [ "$needs" -eq 0 ]; then
      continue
    fi
    checked=$((checked + 1))
    if [ "$setup" -eq 0 ]; then
      echo "error: ${f}: job ${job} runs uv, or tests that run it, without astral-sh/setup-uv" >&2
      failed=1
    fi
  done < <(yq -r "$query" "$f")
done

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "All ${checked} jobs that need uv install it with astral-sh/setup-uv."
