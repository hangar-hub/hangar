#!/usr/bin/env bash
# Records the 30-second terminal demo on the docs Home page (docs/spec/12):
# hangar whoami, a Pull from huggingface.co, the hf CLI downloading through
# Hangar and hangar info. It shows no web UI, so it does not change with the
# UI's look.
# It writes docs/assets/demo.gif, so run it against a real Instance that can
# reach huggingface.co.
#
# Usage: HANGAR_DEMO_TOKEN=hgr_... scripts/demo.sh <url>
#
# The Token needs the write scope. The script signs in with a temporary
# configuration and Hugging Face cache, so it leaves your own alone, and the
# Token is never shown in the recording.
#
# Needs bash, curl, asciinema 2.4 or later, agg, hangar and hf on PATH.
set -euo pipefail

repo="hf-internal-testing/tiny-random-bert"

# type_and_run prints a command as if typed, then runs it.
type_and_run() {
  local i
  printf '\033[1;32m$\033[0m '
  for ((i = 0; i < ${#1}; i++)); do
    printf '%s' "${1:i:1}"
    sleep 0.03
  done
  printf '\n'
  eval "$1"
  sleep 1.5
}

if [[ "${1:-}" == "--play" ]]; then
  # The recorded session. The caller has signed in already.
  clear
  type_and_run "hangar whoami"
  type_and_run "hangar pull $repo"
  # These two are typed as written and expanded only when run.
  # shellcheck disable=SC2016
  type_and_run 'eval "$(hangar env)"'
  # shellcheck disable=SC2016
  type_and_run 'echo "$HF_ENDPOINT"'
  type_and_run "hf download $repo config.json"
  type_and_run "hangar info $repo"
  exit 0
fi

case "${1:-}" in
  -h|--help|"") sed -n '2,15p' "$0"; exit 0 ;;
esac
url="${1%/}"
: "${HANGAR_DEMO_TOKEN:?set HANGAR_DEMO_TOKEN to a write Token for $url}"
for tool in curl asciinema agg hangar hf; do
  command -v "$tool" >/dev/null || { echo "demo: $tool is not on PATH" >&2; exit 1; }
done
curl -fsS "$url/readyz" >/dev/null || { echo "demo: $url/readyz is not ready" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export XDG_CONFIG_HOME="$work/config" HF_HOME="$work/hf" HF_HUB_CACHE="$work/hf/hub"
export HANGAR_CLI_NO_KEYCHAIN=1 HANGAR_CLI_URL="" HANGAR_CLI_TOKEN="" HF_TOKEN="" HF_ENDPOINT=""
printf '%s\n' "$HANGAR_DEMO_TOKEN" | hangar login --url "$url" --token - >/dev/null

root="$(cd "$(dirname "$0")/.." && pwd)"
asciinema rec --overwrite --quiet --cols 100 --rows 28 \
  --command "$root/scripts/demo.sh --play" "$work/demo.cast"
mkdir -p "$root/docs/assets"
agg --font-size 16 "$work/demo.cast" "$root/docs/assets/demo.gif"
echo "Wrote docs/assets/demo.gif. Show it on the Home page under the introduction:"
echo '![Hangar in 30 seconds in a terminal: check the sign-in, pull a model from huggingface.co, download it with the hf CLI and show its details](assets/demo.gif)'
