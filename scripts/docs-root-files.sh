#!/usr/bin/env bash
# Keeps the files the docs Worker needs at the root of the gh-pages branch,
# which Workers Builds deploys (docs/spec/12-packaging-and-release.md): its
# wrangler.jsonc and .assetsignore, and the _headers, _redirects and root
# 404.html it serves the site with. mike publishes each docs version into its
# own directory and leaves root files alone, so this copies every file in
# deploy/cloudflare/docs-root/, dotfiles included, to the root and commits
# only when one is missing or differs. Run it after `mike deploy --push`,
# with gh-pages fetched.
#
# mike's latest alias is a symlink to a version directory, so the site serves
# that version's pages at two addresses, such as /1.4/faq/ and /latest/faq/.
# Search engines then sometimes index the versioned one rather than the
# canonical /latest/ one, so this adds rules to _redirects that send the
# version that latest names to /latest/.
# Usage: scripts/docs-root-files.sh [--push]
set -euo pipefail
cd "$(dirname "$0")/.."

src=deploy/cloudflare/docs-root
if ! git rev-parse --verify --quiet gh-pages >/dev/null; then
  echo "error: no gh-pages branch; run mike deploy first" >&2
  exit 1
fi

# Stage the files on top of gh-pages in a throwaway index, so the working
# tree and the checked-out branch stay as they are.
index="$(mktemp)"
trap 'rm -f "$index"' EXIT
GIT_INDEX_FILE="$index" git read-tree gh-pages
latest=""
if [ "$(git ls-tree gh-pages latest | cut -d' ' -f1)" = 120000 ]; then
  latest="$(git cat-file blob gh-pages:latest)"
fi
shopt -s dotglob
for f in "$src"/*; do
  if [ "$(basename "$f")" = _redirects ] && [ -n "$latest" ]; then
    blob="$({
      cat "$f"
      printf '\n# Added by scripts/docs-root-files.sh: /%s/ is the version that /latest/ names.\n' "$latest"
      printf '/%s      /latest/          301\n' "$latest"
      printf '/%s/*    /latest/:splat    301\n' "$latest"
    } | git hash-object -w --stdin)"
  else
    blob="$(git hash-object -w "$f")"
  fi
  GIT_INDEX_FILE="$index" git update-index --add --cacheinfo "100644,${blob},$(basename "$f")"
done
tree="$(GIT_INDEX_FILE="$index" git write-tree)"
if [ "$tree" = "$(git rev-parse 'gh-pages^{tree}')" ]; then
  echo "gh-pages already has the root files from ${src}."
  exit 0
fi
commit="$(git commit-tree "$tree" -p gh-pages -m "Update the root files from ${src}")"
git update-ref refs/heads/gh-pages "$commit"
echo "gh-pages now has the root files from ${src}."

if [ "${1:-}" = "--push" ]; then
  git push origin gh-pages
fi
