#!/usr/bin/env bash
# Keeps the custom domain file, CNAME, at the root of the gh-pages branch.
# mike publishes each docs version into its own directory, so a CNAME in docs/
# would land in that directory, where GitHub Pages does not read it. GitHub
# Pages, publishing from the gh-pages branch, reads the domain from the root
# CNAME. mike leaves root files alone, so this commits one only when it is
# missing or names another domain. The domain is the host in site_url in
# mkdocs.yml. Run it after `mike deploy --push`, with gh-pages fetched.
# Usage: scripts/docs-cname.sh [--push]
set -euo pipefail
cd "$(dirname "$0")/.."

domain="$(sed -n -E 's#^site_url: https://([^/]+)/.*#\1#p' mkdocs.yml)"
if [ -z "$domain" ]; then
  echo "error: no https site_url in mkdocs.yml" >&2
  exit 1
fi

if [ "$(git show gh-pages:CNAME 2>/dev/null || true)" = "$domain" ]; then
  echo "gh-pages already serves ${domain}."
  exit 0
fi

# Commit CNAME on top of gh-pages with a throwaway index, so the working tree
# and the checked-out branch stay as they are.
index="$(mktemp)"
trap 'rm -f "$index"' EXIT
blob="$(printf '%s\n' "$domain" | git hash-object -w --stdin)"
GIT_INDEX_FILE="$index" git read-tree gh-pages
GIT_INDEX_FILE="$index" git update-index --add --cacheinfo "100644,${blob},CNAME"
tree="$(GIT_INDEX_FILE="$index" git write-tree)"
commit="$(git commit-tree "$tree" -p gh-pages -m "Serve the documentation at ${domain}")"
git update-ref refs/heads/gh-pages "$commit"
echo "gh-pages now serves ${domain}."

if [ "${1:-}" = "--push" ]; then
  git push origin gh-pages
fi
