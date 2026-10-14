#!/usr/bin/env bash
# Runs every Go fuzz target for the given time each (default 10m).
# Usage: scripts/fuzz.sh [duration]
set -euo pipefail
duration="${1:-10m}"
cd "$(dirname "$0")/.."
found=0
# The file and target lists are read on file descriptors 3 and 4, so nothing
# that go test runs can consume them from standard input.
while IFS= read -r file <&3; do
  pkg="./$(dirname "$file")"
  while IFS= read -r target <&4; do
    found=$((found + 1))
    echo "== $pkg $target ($duration)"
    # Go minimises every input that finds new coverage, for up to 60s by
    # default and in time that grows with the square of the input's size.
    # On binary formats (Parquet) that left the fuzzer running almost no
    # inputs, so cap minimisation at 2,000 tries.
    go test -run '^$' -fuzz "^${target}\$" -fuzztime "$duration" -fuzzminimizetime 2000x "$pkg"
  done 4< <(grep -oE '^func Fuzz[A-Za-z0-9_]+' "$file" | awk '{print $2}')
done 3< <(grep -rl --include='*_test.go' -E '^func Fuzz' cmd internal compat 2>/dev/null | sort)
echo "Ran $found fuzz targets."
