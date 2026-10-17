#!/usr/bin/env bash
# Fails when a Go test returns early, and so reports PASS, instead of
# calling t.Skip when something it needs is missing:
#
#   - an `if` on an Enabled() check or a HANGAR_TEST_ variable whose body
#     only logs and returns, and
#   - any `if` in a test file whose body only logs with t.Log or t.Logf
#     and returns.
#
# Use dbtest.RequirePostgres, blobtest.RequireS3 or t.Skipf with the
# reason, so `go test -v` shows the skip; make test and make test-compat
# set what the tests need, so nothing is skipped there.
#
# Usage: scripts/check-silent-skips.sh [dir]...   (default: cmd internal compat deploy)
set -euo pipefail
cd "$(dirname "$0")/.."
dirs=("$@")
[ "${#dirs[@]}" -gt 0 ] || dirs=(cmd internal compat deploy)

found=0
while IFS= read -r -d '' f; do
  case "$f" in
    *_test.go | */*test/*.go) ;;
    *) continue ;;
  esac
  if out="$(perl -0777 -ne '
    my $body = qr/(?:[ \t]*(?:\/\/[^\n]*|t\.Logf?\([^\n]*\))\n)*[ \t]*return\n[ \t]*\}/;
    while (/^([ \t]*if [^\n]*(?:Enabled\([^\n)]*\)|Getenv\("HANGAR_TEST_)[^\n]*\{\n$body)/mg) {
      my $pre = substr($_, 0, $-[0]); my $n = ($pre =~ tr/\n//) + 1; print "$ARGV:$n\n";
    }
    while (/^([ \t]*if [^\n]*\{\n(?:[ \t]*\/\/[^\n]*\n)*[ \t]*t\.Logf?\([^\n]*\)\n$body)/mg) {
      my $pre = substr($_, 0, $-[0]); my $n = ($pre =~ tr/\n//) + 1; print "$ARGV:$n\n";
    }
  ' "$f" | sort -u)" && [ -n "$out" ]; then
    while IFS= read -r loc; do
      echo "error: ${loc}: the test returns early and reports PASS; call t.Skipf with the reason (or dbtest.RequirePostgres, blobtest.RequireS3)" >&2
    done <<<"$out"
    found=1
  fi
done < <(find "${dirs[@]}" -name '*.go' -not -path '*/node_modules/*' -print0)
exit "$found"
