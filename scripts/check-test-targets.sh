#!/usr/bin/env bash
# Checks how the test targets run, without Docker:
#
#   - Every `go test` or scripts/coverage.sh run in the Makefile and the
#     workflows whose packages include an S3 Blob store test goes through
#     scripts/with-test-minio.sh, so those tests use one MinIO container
#     the script owns instead of one testcontainers' reaper can remove
#     mid-run.
#   - `make test` runs the UI tests even when the Go tests fail, and still
#     fails.
# Usage: scripts/check-test-targets.sh
set -euo pipefail
cd "$(dirname "$0")/.."

failed=0

# Packages with tests that use the S3 test server, as ./dir paths.
s3pkgs=()
while IFS= read -r dir; do
  s3pkgs+=("./$dir")
done < <(grep -rlE 'blobtest\.(S3|S3TestServer)\(' --include='*_test.go' cmd internal compat \
  | xargs -n1 dirname | sort -u)
if [ "${#s3pkgs[@]}" -eq 0 ]; then
  echo "error: found no package that uses the S3 test server" >&2
  exit 1
fi

# covers reports whether a go test package pattern includes package dir.
covers() {
  local pattern="${1%/}" dir="$2"
  case "$pattern" in
    ./...) return 0 ;;
    */...) [[ "$dir" == "${pattern%/...}" || "$dir" == "${pattern%/...}"/* ]] ;;
    *) [[ "$dir" == "$pattern" ]] ;;
  esac
}

checked=0
for f in Makefile .github/workflows/*.yml; do
  while IFS= read -r line; do
    runs_cover=0
    if printf '%s\n' "$line" | grep -Eq '(^|[^A-Za-z0-9_])scripts/coverage\.sh'; then
      runs_cover=1
    elif ! printf '%s\n' "$line" | grep -Eq '(^|[^A-Za-z0-9_])(go|\$\(GO\)) test '; then
      continue
    fi
    needs=$runs_cover
    if [ "$needs" -eq 0 ]; then
      read -ra words <<<"$line"
      for word in "${words[@]}"; do
        [[ "$word" == ./* ]] || continue
        for dir in "${s3pkgs[@]}"; do
          if covers "$word" "$dir"; then needs=1; fi
        done
      done
    fi
    [ "$needs" -eq 1 ] || continue
    checked=$((checked + 1))
    if ! printf '%s\n' "$line" | grep -Eq 'scripts/with-test-minio\.sh .*((go|\$\(GO\)) test |scripts/coverage\.sh)'; then
      echo "error: ${f}: runs S3 Blob store tests without scripts/with-test-minio.sh: $(printf '%s' "$line" | sed 's/^[[:space:]]*//')" >&2
      failed=1
    fi
  done < <(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$f")
done
echo "${checked} test runs that include S3 Blob store tests checked."

# `make test` with a failing Go suite: the UI suite must still run. GO=false
# fails test-go at once, PNPM=echo prints the UI commands instead of running
# them, and the test service variables keep the wrappers from starting
# containers.
out="$(make --no-print-directory test GO=false PNPM='echo pnpm' \
  HANGAR_TEST_POSTGRES=0 HANGAR_TEST_MINIO=0 2>&1)" && status=0 || status=$?
if [ "$status" -eq 0 ]; then
  echo "error: make test passed although the Go tests failed" >&2
  failed=1
fi
if ! printf '%s\n' "$out" | grep -q '^pnpm run test$'; then
  echo "error: make test did not run the UI tests after the Go tests failed:" >&2
  printf '%s\n' "$out" >&2
  failed=1
fi

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "make test runs the UI tests after a Go test failure and fails."
