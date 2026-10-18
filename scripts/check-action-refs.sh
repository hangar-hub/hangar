#!/usr/bin/env bash
# Checks that every GitHub Action a workflow uses is pinned to a full commit
# SHA with the release it came from in a comment, for example
#   uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
# and that the tag in the comment exists and points at that commit, so the
# comment Dependabot updates says what runs. A tag can be moved; a SHA
# cannot. actionlint checks neither. Needs network access to github.com.
# Usage: scripts/check-action-refs.sh [workflow files...]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then
  set -- .github/workflows/*.yml
fi

failed=0
checked=0
while IFS= read -r line <&3; do
  use="$(echo "$line" | sed -E 's/^[[:space:]-]*uses:[[:space:]]*([^[:space:]#]+).*/\1/')"
  case "$use" in
    ./* | docker://*) continue ;;
  esac
  checked=$((checked + 1))
  action="${use%@*}"
  ref="${use#*@}"
  # owner/repo/path@ref lives in owner/repo.
  repo="$(echo "$action" | cut -d/ -f1-2)"
  if ! [[ "$ref" =~ ^[0-9a-f]{40}$ ]]; then
    echo "error: ${use}: pin to a full commit SHA, with the release in a comment (# vX.Y.Z)" >&2
    failed=1
    continue
  fi
  if ! [[ "$line" =~ \#[[:space:]]*(v[0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$ ]]; then
    echo "error: ${use}: add the release the SHA belongs to as a comment, such as # v1.2.3" >&2
    failed=1
    continue
  fi
  tag="${BASH_REMATCH[1]}"
  if ! out="$(git ls-remote "https://github.com/${repo}.git" "refs/tags/${tag}" "refs/tags/${tag}^{}")"; then
    echo "error: cannot list refs of ${repo}" >&2
    failed=1
    continue
  fi
  if [ -z "$out" ]; then
    echo "error: ${use}: ${repo} has no tag ${tag}" >&2
    failed=1
    continue
  fi
  # An annotated tag lists the tag object and, as ^{}, the commit it points at.
  commit="$(echo "$out" | awk -v peeled="refs/tags/${tag}^{}" '$2 == peeled {print $1}')"
  if [ -z "$commit" ]; then
    commit="$(echo "$out" | awk '{print $1}' | head -1)"
  fi
  if [ "$commit" != "$ref" ]; then
    echo "error: ${use}: ${tag} in ${repo} is ${commit}, not ${ref}" >&2
    failed=1
  fi
done 3< <(grep -hE '^[[:space:]-]*uses:[[:space:]]*[^[:space:]#]+' "$@" | sed -E 's/^[[:space:]]+//' | sort -u)

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "All ${checked} action refs are commit SHAs that match their release tags."
