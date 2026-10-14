#!/usr/bin/env bash
# Smoke test for a running Instance (docs/spec/13-testing.md): health, sign
# in, create a Repo, upload, download, git clone, and Pull a tiny Repo from
# huggingface.co. CI runs it against every Compose example and the Helm
# chart on kind.
#
# Usage: scripts/smoke.sh [--skip STEP]... [--only STEP]... <url>
#
# Steps: health, login, create, upload, download, clone, pull.
# Sign-in uses HANGAR_SMOKE_TOKEN if set, otherwise HANGAR_SMOKE_USERNAME
# (default hangar-admin) and HANGAR_SMOKE_PASSWORD, and creates a Token.
# Skip `pull` on machines without internet access.
#
# Needs bash, curl, jq, git and base64.
set -euo pipefail

skip=()
only=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip) skip+=("$2"); shift 2 ;;
    --only) only+=("$2"); shift 2 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) break ;;
  esac
done
url="${1:?usage: scripts/smoke.sh [--skip STEP]... [--only STEP]... <url>}"
url="${url%/}"

want() {
  local s
  for s in "${skip[@]+"${skip[@]}"}"; do [[ "$s" == "$1" ]] && return 1; done
  [[ ${#only[@]} -eq 0 ]] && return 0
  for s in "${only[@]}"; do [[ "$s" == "$1" ]] && return 0; done
  return 1
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
pass() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s: %s\n' "$1" "$2" >&2; exit 1; }

# curl with the Token, failing on HTTP errors, never printing the Token.
api() { curl -fsS -H "Authorization: Bearer ${token}" "$@"; }

wait_ready() {
  for _ in $(seq 1 60); do
    if curl -fsS "$url/readyz" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  return 1
}

if want health; then
  wait_ready || fail health "/readyz did not answer 200 within 2 minutes"
  [[ "$(curl -fsS "$url/healthz" | jq -r .status)" == ok ]] || fail health "/healthz is not ok"
  pass health
fi

token="${HANGAR_SMOKE_TOKEN:-}"
username="${HANGAR_SMOKE_USERNAME:-hangar-admin}"
if want login; then
  if [[ -z "$token" ]]; then
    : "${HANGAR_SMOKE_PASSWORD:?set HANGAR_SMOKE_TOKEN, or HANGAR_SMOKE_PASSWORD for $username}"
    jar="$work/cookies"
    me="$(jq -n --arg u "$username" --arg p "$HANGAR_SMOKE_PASSWORD" '{username: $u, password: $p}' |
      curl -fsS -c "$jar" -H 'Content-Type: application/json' --data @- "$url/api/hangar/v1/auth/login")" ||
      fail login "password sign-in failed"
    csrf="$(jq -r '.csrf_token // empty' <<<"$me")"
    token="$(curl -fsS -b "$jar" -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" \
      --data '{"name": "smoke test", "scope": "write"}' "$url/api/hangar/v1/me/tokens" | jq -r .secret)" ||
      fail login "creating a Token failed"
  fi
  username="$(api "$url/api/whoami-v2" | jq -r .name)" || fail login "whoami-v2 rejected the Token"
  pass "login as $username"
fi

repo="$username/smoke-$(date +%s)-$RANDOM"
content="Smoke test $(date -u +%FT%TZ)"
if want create; then
  jq -n --arg name "${repo#*/}" --arg org "$username" '{name: $name, organization: $org, type: "model", private: true}' |
    api -H 'Content-Type: application/json' --data @- "$url/api/repos/create" >/dev/null || fail create "POST /api/repos/create"
  pass "create $repo"
fi

if want upload; then
  b64="$(printf '%s' "$content" | base64 | tr -d '\n')"
  {
    jq -cn '{key: "header", value: {summary: "Smoke test upload", description: ""}}'
    jq -cn --arg c "$b64" '{key: "file", value: {path: "smoke.txt", content: $c, encoding: "base64"}}'
  } | api -H 'Content-Type: application/x-ndjson' --data-binary @- "$url/api/models/$repo/commit/main" >/dev/null ||
    fail upload "POST /api/models/$repo/commit/main"
  pass upload
fi

if want download; then
  got="$(api -L "$url/$repo/resolve/main/smoke.txt")" || fail download "GET /resolve"
  [[ "$got" == "$content" ]] || fail download "downloaded content differs"
  pass download
fi

if want clone; then
  proto="${url%%://*}"
  hostpath="${url#*://}"
  GIT_TERMINAL_PROMPT=0 git clone -q "$proto://${username}:${token}@${hostpath}/$repo.git" "$work/clone" 2>/dev/null ||
    fail clone "git clone"
  [[ "$(cat "$work/clone/smoke.txt")" == "$content" ]] || fail clone "cloned content differs"
  pass "git clone"
fi

if want pull; then
  upstream_repo="${HANGAR_SMOKE_PULL_REPO:-hf-internal-testing/tiny-random-gpt2}"
  job="$(jq -n --arg r "$upstream_repo" '{upstream: "huggingface", repo: $r, type: "model", selection: {include: ["config.json"]}}' |
    api -H 'Content-Type: application/json' --data @- "$url/api/hangar/v1/pulls" | jq -r .id)" || fail pull "POST /pulls"
  for _ in $(seq 1 90); do
    status="$(api "$url/api/hangar/v1/jobs/$job" | jq -r .status)"
    case "$status" in
      succeeded) break ;;
      failed|cancelled) fail pull "the Pull $status" ;;
    esac
    sleep 2
  done
  [[ "$status" == succeeded ]] || fail pull "the Pull did not finish within 3 minutes"
  api -o /dev/null "$url/$upstream_repo/resolve/main/config.json" || fail pull "the pulled file does not download"
  pass "pull $upstream_repo"
fi

echo "Smoke test passed against $url"
