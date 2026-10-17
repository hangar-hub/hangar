#!/usr/bin/env bash
# Runs a command against one Postgres container that every test package
# shares, as CI's service container is shared, instead of a container per
# package. It starts the container, exports HANGAR_TEST_POSTGRES_DSN, runs
# the command and removes the container however the command ends.
#
# When HANGAR_TEST_POSTGRES_DSN is already set, or HANGAR_TEST_POSTGRES is
# not 1, it runs the command unchanged.
# Usage: scripts/with-test-postgres.sh command [args...]
set -euo pipefail

if [[ -n "${HANGAR_TEST_POSTGRES_DSN:-}" || "${HANGAR_TEST_POSTGRES:-}" != "1" ]]; then
  exec "$@"
fi

# Keep in step with dbtest.PostgresImage.
image=postgres:18-alpine
name="hangar-test-postgres-$$"

cleanup() { docker rm -fv "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# fsync=off as the testcontainers Postgres module sets it. Every package
# runs its tests against this one server, so allow more connections than
# the default 100.
docker run -d --rm --name "$name" --label dev.hangar.test=postgres \
  -e POSTGRES_USER=hangar -e POSTGRES_PASSWORD=hangar -e POSTGRES_DB=postgres \
  -p 127.0.0.1::5432 "$image" \
  -c fsync=off -c max_connections=500 >/dev/null

# The image's entrypoint first runs a server on the Unix socket only, so
# wait for TCP.
for _ in $(seq 1 120); do
  if docker exec "$name" pg_isready -q -h 127.0.0.1 -U hangar 2>/dev/null; then
    break
  fi
  sleep 1
done
docker exec "$name" pg_isready -q -h 127.0.0.1 -U hangar \
  || { echo "Test Postgres did not start:"; docker logs "$name"; exit 1; }

port=$(docker port "$name" 5432/tcp | head -n 1)
port=${port##*:}
export HANGAR_TEST_POSTGRES_DSN="postgres://hangar:hangar@127.0.0.1:${port}/postgres?sslmode=disable"

"$@"
