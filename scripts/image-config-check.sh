#!/usr/bin/env bash
# Checks how the container image reads its configuration file: the
# /etc/hangar/hangar.yaml it ships sets nothing, and a file mounted at that
# path is read with no HANGAR_CONFIG on the command line.
# Usage: scripts/image-config-check.sh <image>
set -euo pipefail
image="${1:?usage: scripts/image-config-check.sh <image>}"

# A fixed test value for security.instance_key (32 bytes, base64), so
# config validate has no missing key to report.
set_args=(--set server.public_url=http://localhost:8080
  --set security.instance_key=MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=)

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 1. The file the image ships parses and leaves every default alone.
docker run --rm "$image" config validate --json "${set_args[@]}" >"$tmp/default.json"
if ! grep -q '"valid": true' "$tmp/default.json"; then
  echo "error: the image's own /etc/hangar/hangar.yaml did not validate:" >&2
  cat "$tmp/default.json" >&2
  exit 1
fi
echo "ok: the shipped /etc/hangar/hangar.yaml validates"

# 2. A file mounted at /etc/hangar/hangar.yaml is read. Its storage.backend
# is invalid on purpose, so validation must fail and name the key.
printf 'storage:\n  backend: tape\n' >"$tmp/hangar.yaml"
chmod 0644 "$tmp/hangar.yaml"
if docker run --rm -v "$tmp/hangar.yaml:/etc/hangar/hangar.yaml:ro" "$image" \
  config validate --json "${set_args[@]}" >"$tmp/mounted.json" 2>/dev/null; then
  echo "error: a mounted /etc/hangar/hangar.yaml was not read" >&2
  exit 1
fi
if ! grep -q 'storage.backend' "$tmp/mounted.json"; then
  echo "error: validation failed, but not on the mounted file's storage.backend:" >&2
  cat "$tmp/mounted.json" >&2
  exit 1
fi
echo "ok: a file mounted at /etc/hangar/hangar.yaml is read"
