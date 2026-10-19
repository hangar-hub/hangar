#!/usr/bin/env bash
# Builds this branch's documentation into site/ and publishes it as a Preview
# of the docs Worker, hangar-docs (docs/spec/12-packaging-and-release.md).
# Workers Builds runs it as the preview command for every branch except
# gh-pages, so it needs only what the build image has: bash, Python with pip,
# and Node with npx. The Preview is named after the branch
# (scripts/preview-name.sh) and served at https://<name>.docs.hangar.page/.
# Usage: scripts/docs-preview.sh [--build-only]
set -euo pipefail
cd "$(dirname "$0")/.."

# Keep hooks.py bytecode out of docs/, which mkdocs would publish.
export PYTHONDONTWRITEBYTECODE=1
venv=.docs-venv
python3 -m venv "$venv"
"$venv/bin/pip" install --quiet --disable-pip-version-check -r docs/requirements.txt
"$venv/bin/mkdocs" build --strict --site-dir site
# The production site's headers, and noindex, which Cloudflare adds only to
# workers.dev Previews; the production redirects send versionless paths to
# /latest/, which a Preview does not have.
cp deploy/cloudflare/docs-root/_headers site/_headers
printf '\n# Previews stay out of search results.\n/*\n  X-Robots-Tag: noindex\n' >>site/_headers

if [ "${1:-}" = "--build-only" ]; then
  exit 0
fi

name="$(scripts/preview-name.sh)"
wrangler="$(sed -nE 's/.*wrangler@([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' Makefile | head -n1)"
WRANGLER_SEND_METRICS=false npx --yes "wrangler@${wrangler}" preview \
  --config deploy/cloudflare/docs-preview/wrangler.jsonc --name "$name" \
  --message "$(git log -1 --format=%s)"
