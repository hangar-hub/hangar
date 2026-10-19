#!/usr/bin/env bash
# Builds this branch's documentation into site/ and publishes it as a Preview
# of the docs Worker, hangar-docs (docs/spec/12-packaging-and-release.md).
# Workers Builds runs it as the preview command for every branch except
# gh-pages, so it needs only what the build image has: bash, Python with pip,
# and Node with npx. The Preview is named after the branch and served at
# https://<branch>-hangar-docs.<account subdomain>.workers.dev/.
# Usage: scripts/docs-preview.sh [--build-only]
set -euo pipefail
cd "$(dirname "$0")/.."

# Keep hooks.py bytecode out of docs/, which mkdocs would publish.
export PYTHONDONTWRITEBYTECODE=1
venv=.docs-venv
python3 -m venv "$venv"
"$venv/bin/pip" install --quiet --disable-pip-version-check -r docs/requirements.txt
"$venv/bin/mkdocs" build --strict --site-dir site
# The production site's headers; its redirects send versionless paths to
# /latest/, which a Preview does not have.
cp deploy/cloudflare/docs-root/_headers site/_headers

if [ "${1:-}" = "--build-only" ]; then
  exit 0
fi

# Workers Builds checks out a commit, not a branch, and names the branch in
# WORKERS_CI_BRANCH. A Preview name is a DNS label: with "-hangar-docs" it
# must fit in 63 characters.
branch="${WORKERS_CI_BRANCH:-$(git rev-parse --abbrev-ref HEAD)}"
name="$(printf '%s' "$branch" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed -E 's/-+/-/g; s/^-//; s/-$//' | cut -c1-50 | sed -E 's/-$//')"
wrangler="$(sed -nE 's/.*wrangler@([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' Makefile | head -n1)"
WRANGLER_SEND_METRICS=false npx --yes "wrangler@${wrangler}" preview \
  --config deploy/cloudflare/docs-preview/wrangler.jsonc --name "$name" \
  --message "$(git log -1 --format=%s)"
