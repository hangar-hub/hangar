#!/usr/bin/env bash
# Prints the name of this branch's Preview of a Cloudflare Worker, which is
# also the first label of its address: <name>.docs.hangar.page for the docs
# and <name>.demo.hangar.page for the demo
# (docs/spec/12-packaging-and-release.md). Workers Builds checks out a commit,
# not a branch, and names the branch in WORKERS_CI_BRANCH; elsewhere the
# checked-out branch is used. The name is a DNS label: lower case letters,
# digits and single hyphens, at most 50 characters.
# Usage: scripts/preview-name.sh [branch]
set -euo pipefail

branch="${1:-${WORKERS_CI_BRANCH:-$(git rev-parse --abbrev-ref HEAD)}}"
name="$(printf '%s' "$branch" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' |
  sed -E 's/-+/-/g; s/^-//; s/-$//' | cut -c1-50 | sed -E 's/-$//')"
if [ -z "$name" ]; then
  echo "error: branch '${branch}' gives an empty Preview name" >&2
  exit 1
fi
printf '%s\n' "$name"
