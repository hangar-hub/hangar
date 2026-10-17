#!/usr/bin/env bash
# Runs a `go test` command and, if packages failed to build because files
# vanished from the Go build cache during the run, runs those packages once
# more and says so.
#
# The go command looks a cached build output up when it plans a build and
# opens the file later, when the compiler, linker or vet needs it. Anything
# that deletes files from GOCACHE in between, such as a disk cleaner or a
# `find "$(go env GOCACHE)" -delete` on a shared machine, makes that package
# fail with `[build failed]` and an error like
#   link: cannot open file .../go-build/3d/3d9dd224...-d: no such file or directory
# although its code is fine. go rebuilds missing entries on the next run, so
# the packages are run again with the same flags. Only packages that failed
# to build are run again, and only when the output shows a GOCACHE file
# missing: a real compile error fails again, and a test failure is never run
# again. The run passes only if every package passes.
#
# Package patterns are the arguments that start with ./ or ../, are ., or
# are import paths under the module; flags and their values are kept.
# Usage: scripts/go-test.sh go test [flags] packages...
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 go test [flags] packages..." >&2
  exit 2
fi

module=github.com/jamesagarside/hangar
out="$(mktemp "${TMPDIR:-/tmp}/hangar-go-test.XXXXXX")"
trap 'rm -f "$out"' EXIT

status=0
"$@" 2>&1 | tee "$out" || status=$?
if [ "$status" -eq 0 ]; then
  exit 0
fi

# The cache the run used. If the go command cannot say, there is nothing to
# match the errors against.
cache="$("$1" env GOCACHE 2>/dev/null)" || exit "$status"
if [ -z "$cache" ] || [ "$cache" = off ]; then
  exit "$status"
fi
# go prints a cache entry's path relative to the package's directory when
# that is shorter, such as ../../../../Library/Caches/go-build/42/42c4...-d,
# so match the cache directory's name and the entry's shape: a two-digit hex
# directory and a SHA-256 name ending in -a or -d.
entry="(^|[[:space:]:/])$(basename "$cache" | sed 's/[][\\.^$*+?(){}|]/\\&/g')/[0-9a-f]{2}/[0-9a-f]{64}-[ad]: no such file or directory"
if ! grep -Eq "$entry" "$out"; then
  exit "$status"
fi

failed=()
while IFS= read -r pkg; do
  failed+=("$pkg")
done < <(sed -n -E 's/^FAIL[[:space:]]+([^[:space:]]+) \[build failed\]$/\1/p' "$out" | sort -u)
if [ "${#failed[@]}" -eq 0 ]; then
  exit "$status"
fi

# Any other failed package keeps the run failed whatever the rerun does.
others=0
if grep -E '^FAIL[[:space:]]' "$out" | grep -vqE '^FAIL[[:space:]]+[^[:space:]]+ \[build failed\]$'; then
  others=1
fi

rerun=()
patterns=0
for arg in "$@"; do
  case "$arg" in
    . | ./* | ../* | "$module" | "$module"/*) patterns=$((patterns + 1)) ;;
    *) rerun+=("$arg") ;;
  esac
done
if [ "$patterns" -eq 0 ]; then
  exit "$status"
fi
rerun+=("${failed[@]}")

{
  echo
  echo "Files in the Go build cache (${cache}) were deleted while this run used them,"
  echo "so these packages failed to build. Their code did not cause it:"
  printf '  %s\n' "${failed[@]}"
  echo "Something outside go removed the files, such as a cache cleaner. Running"
  echo "these packages again. To keep it from recurring, stop the cleaner during"
  echo "test runs or give this run its own GOCACHE (see CONTRIBUTING.md)."
  echo
} >&2

rerun_status=0
"${rerun[@]}" || rerun_status=$?
if [ "$rerun_status" -ne 0 ]; then
  exit "$rerun_status"
fi
if [ "$others" -ne 0 ]; then
  exit "$status"
fi
exit 0
