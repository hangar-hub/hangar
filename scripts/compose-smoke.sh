#!/usr/bin/env bash
# Starts a Compose example, runs the smoke test against it, and removes it
# with its volumes. CI runs it for every file in deploy/compose.
#
# Usage: scripts/compose-smoke.sh <example> [smoke.sh options]
#   scripts/compose-smoke.sh quickstart --skip pull
#
# HANGAR_IMAGE picks the image (default ghcr.io/jamesagarside/hangar:latest).
set -euo pipefail
cd "$(dirname "$0")/.."
example="${1:?usage: scripts/compose-smoke.sh <example> [smoke.sh options]}"
shift
file="deploy/compose/${example}.yaml"
[[ -f "$file" ]] || { echo "no such example: $file" >&2; exit 2; }

# The examples have no default secrets, so generate fresh ones for each run.
export HANGAR_ADMIN_PASSWORD="${HANGAR_ADMIN_PASSWORD:-smoke-$(openssl rand -hex 12)}"
export HANGAR_INSTANCE_KEY="${HANGAR_INSTANCE_KEY:-$(openssl rand -base64 32)}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-$(openssl rand -hex 24)}"
export MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-$(openssl rand -hex 24)}"
export KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-$(openssl rand -hex 24)}"
export HANGAR_OIDC_CLIENT_SECRET="${HANGAR_OIDC_CLIENT_SECRET:-$(openssl rand -hex 24)}"
export HANGAR_PORT="${HANGAR_PORT:-18080}"
export HANGAR_PUBLIC_URL="http://localhost:${HANGAR_PORT}"
project="hangar-smoke-${example}"
compose() { docker compose -p "$project" -f "$file" "$@"; }

cleanup() {
  status=$?
  if [[ $status -ne 0 ]]; then compose logs --no-color --tail 200 || true; fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  exit $status
}
trap cleanup EXIT

compose up -d --wait --wait-timeout 300
HANGAR_SMOKE_PASSWORD="$HANGAR_ADMIN_PASSWORD" ./scripts/smoke.sh "$@" "$HANGAR_PUBLIC_URL"
