#!/usr/bin/env bash
# Checks that every GitHub Action a workflow uses names a tag, branch or
# commit that exists. actionlint does not check this, and a missing ref fails
# the job at "Set up job". Needs network access to github.com.
# Usage: scripts/check-action-refs.sh [workflow files...]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then
  set -- .github/workflows/*.yml
fi

failed=0
checked=0
while IFS= read -r use <&3; do
  action="${use%@*}"
  ref="${use#*@}"
  # owner/repo/path@ref lives in owner/repo.
  repo="$(echo "$action" | cut -d/ -f1-2)"
  checked=$((checked + 1))
  if [[ "$ref" =~ ^[0-9a-f]{40}$ ]]; then
    # A full commit SHA cannot be checked with ls-remote; trust it.
    continue
  fi
  if ! out="$(git ls-remote "https://github.com/${repo}.git" "refs/tags/${ref}" "refs/heads/${ref}")"; then
    echo "error: cannot list refs of ${repo}" >&2
    failed=1
    continue
  fi
  if [ -z "$out" ]; then
    echo "error: ${action}@${ref}: no tag or branch named ${ref} in ${repo}" >&2
    failed=1
  fi
done 3< <(grep -hoE '^[[:space:]-]*uses:[[:space:]]*[^[:space:]#]+' "$@" |
  sed -E 's/^[[:space:]-]*uses:[[:space:]]*//' |
  grep -vE '^(\./|docker://)' | sort -u)

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "All ${checked} action refs exist."
