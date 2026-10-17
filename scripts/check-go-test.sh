#!/usr/bin/env bash
# Checks scripts/go-test.sh against a fake go command, without building
# anything: packages that failed to build because GOCACHE files vanished
# run once more, and nothing else is run again or reported as passing.
# Usage: scripts/check-go-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
wrapper="$PWD/scripts/go-test.sh"

work="$(mktemp -d "${TMPDIR:-/tmp}/hangar-check-go-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

cache="$work/go-build"
mod=github.com/jamesagarside/hangar

# The fake go answers `go env GOCACHE`, logs every other call's arguments,
# and plays the scenario's outputs in turn: the first call prints run1.out
# and exits with run1.status, the second run2.*, and so on.
cat >"$work/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
dir="$(dirname "$0")"
if [ "$1" = env ] && [ "$2" = GOCACHE ]; then
  cat "$dir/gocache"
  exit 0
fi
echo "$*" >>"$dir/calls"
n=$(wc -l <"$dir/calls" | tr -d ' ')
cat "$dir/run$n.out"
exit "$(cat "$dir/run$n.status")"
EOF
chmod +x "$work/go"
printf '%s\n' "$cache" >"$work/gocache"

h1=3d9dd2241c0e5b8f4a7d6e2c9b1f0a3e5d7c9b2a4f6e8d0c1b3a5f7e9d2c4b6a
h2=42c4e2049977eb3f45cc12587074432e858168ebdb4fe5ba294e761b8461ab77
# go prints an absolute path, or one relative to the package's directory
# when that is shorter, as in this vet error from a real run.
lost="link: cannot open file ${cache}/3d/${h1}-d: no such file or directory"
vet="vet: in ${mod}/internal/datasets/viewer, can't import facts for package \"net/http/httptest\": open ../../../../$(basename "$cache")/42/${h2}-d: no such file or directory"

failed=0
fail() {
  echo "error: $*" >&2
  failed=1
}

# scenario NAME: resets the fake go's calls and outputs.
scenario() {
  name="$1"
  rm -f "$work"/calls "$work"/run*.out "$work"/run*.status
}

# play N STATUS OUTPUT: the fake go's Nth call prints OUTPUT and exits STATUS.
play() {
  printf '%s\n' "$3" >"$work/run$1.out"
  echo "$2" >"$work/run$1.status"
}

# run: runs the wrapper as make test-go does; sets got (exit status), calls.
run() {
  got=0
  "$wrapper" "$work/go" test -race -count=1 -timeout 30m -tags integration ./... >"$work/output" 2>&1 || got=$?
  calls="$(cat "$work/calls" 2>/dev/null || true)"
}

expect_calls() {
  if [ "$calls" != "$1" ]; then
    fail "$name: go was called with"
    printf '%s\n' "$calls" >&2
    echo "want:" >&2
    printf '%s\n' "$1" >&2
  fi
}

expect_status() {
  if [ "$got" -ne "$1" ]; then
    fail "$name: exit status $got, want $1"
    cat "$work/output" >&2
  fi
}

first="test -race -count=1 -timeout 30m -tags integration ./..."

scenario "cache files lost, rerun passes"
play 1 1 "ok  	${mod}/internal/repo	3.1s
# ${mod}/cmd/hangar.test
${lost}
# ${mod}/internal/datasets/viewer
${vet}
FAIL	${mod}/cmd/hangar [build failed]
FAIL	${mod}/internal/datasets/viewer [build failed]
FAIL"
play 2 0 "ok  	${mod}/cmd/hangar	48.9s
ok  	${mod}/internal/datasets/viewer	26.9s"
run
expect_status 0
expect_calls "$first
test -race -count=1 -timeout 30m -tags integration ${mod}/cmd/hangar ${mod}/internal/datasets/viewer"
if ! grep -q "were deleted while this run used them" "$work/output"; then
  fail "$name: the wrapper did not say why it ran the packages again"
fi

scenario "cache files lost, rerun fails"
play 1 1 "# ${mod}/cmd/hangar.test
${lost}
FAIL	${mod}/cmd/hangar [build failed]"
play 2 1 "--- FAIL: TestServe (0.01s)
FAIL	${mod}/cmd/hangar	1.2s"
run
expect_status 1
expect_calls "$first
test -race -count=1 -timeout 30m -tags integration ${mod}/cmd/hangar"

scenario "cache files lost alongside a test failure"
play 1 1 "--- FAIL: TestPush (0.20s)
FAIL	${mod}/internal/repo	3.1s
# ${mod}/cmd/hangar.test
${lost}
FAIL	${mod}/cmd/hangar [build failed]"
play 2 0 "ok  	${mod}/cmd/hangar	48.9s"
run
expect_status 1
expect_calls "$first
test -race -count=1 -timeout 30m -tags integration ${mod}/cmd/hangar"

scenario "a compile error is not run again"
play 1 1 "# ${mod}/cmd/hangar
cmd/hangar/main.go:12:2: undefined: serve
FAIL	${mod}/cmd/hangar [build failed]"
run
expect_status 1
expect_calls "$first"

scenario "a relative path to a lost cache file"
play 1 1 "# ${mod}/internal/hfapi
# [${mod}/internal/hfapi]
${vet}
FAIL	${mod}/internal/hfapi [build failed]"
play 2 0 "ok  	${mod}/internal/hfapi	12.0s"
run
expect_status 0
expect_calls "$first
test -race -count=1 -timeout 30m -tags integration ${mod}/internal/hfapi"

scenario "a missing file outside GOCACHE is not run again"
play 1 1 "# ${mod}/internal/web
internal/web/embed.go:9:12: pattern dist: open /src/internal/web/dist: no such file or directory
FAIL	${mod}/internal/web [build failed]"
run
expect_status 1
expect_calls "$first"

scenario "a test failure is not run again"
play 1 1 "--- FAIL: TestPush (0.20s)
FAIL	${mod}/internal/repo	3.1s"
run
expect_status 1
expect_calls "$first"

scenario "a passing run runs once"
play 1 0 "ok  	${mod}/internal/repo	3.1s"
run
expect_status 0
expect_calls "$first"

if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "scripts/go-test.sh runs packages again only when GOCACHE files vanished."
