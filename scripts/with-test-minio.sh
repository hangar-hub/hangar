#!/usr/bin/env bash
# Runs a command against one MinIO container that every test package
# shares, the way scripts/with-test-postgres.sh does for Postgres. It
# starts the container, exports HANGAR_TEST_S3_ENDPOINT and the other
# HANGAR_TEST_S3_* variables, runs the command and removes the container
# however the command ends.
#
# Without it, each package that needs S3 starts MinIO with testcontainers,
# and testcontainers' reaper (Ryuk) owns the container's lifetime. In a
# parallel `go test ./...` run every package shares one reaper; a package
# whose reaper handshake fails ("Reaper handshake failed: read ack: EOF",
# which testcontainers only logs) holds no connection to it, so the reaper
# removes that package's MinIO once the other packages finish, while its
# tests still run. This container belongs to the script, not the reaper.
#
# When HANGAR_TEST_S3_ENDPOINT is already set, or HANGAR_TEST_MINIO is not
# 1, it runs the command unchanged.
# Usage: scripts/with-test-minio.sh command [args...]
set -euo pipefail

if [[ -n "${HANGAR_TEST_S3_ENDPOINT:-}" || "${HANGAR_TEST_MINIO:-}" != "1" ]]; then
  exec "$@"
fi

# Keep in step with blobtest.MinIOImage (TestWrapperStartsMinIOImage
# checks it).
image="${HANGAR_TEST_MINIO_IMAGE:-pgsty/minio:RELEASE.2026-08-04T00-00-00Z}"
name="hangar-test-minio-$$"
user=hangar
password=hangar-secret

cleanup() { docker rm -fv "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker run -d --rm --name "$name" --label dev.hangar.test=minio \
  -e MINIO_ROOT_USER="$user" -e MINIO_ROOT_PASSWORD="$password" \
  -p 127.0.0.1::9000 "$image" server /data >/dev/null

port=$(docker port "$name" 9000/tcp | head -n 1)
port=${port##*:}
endpoint="http://127.0.0.1:${port}"

for _ in $(seq 1 120); do
  if curl -fsS -o /dev/null "$endpoint/minio/health/live" 2>/dev/null; then
    break
  fi
  sleep 1
done
curl -fsS -o /dev/null "$endpoint/minio/health/live" \
  || { echo "Test MinIO did not start:"; docker logs "$name"; exit 1; }

export HANGAR_TEST_S3_ENDPOINT="$endpoint"
export HANGAR_TEST_S3_ACCESS_KEY="$user"
export HANGAR_TEST_S3_SECRET_KEY="$password"
export HANGAR_TEST_S3_BUCKET=hangar-test
export HANGAR_TEST_S3_REGION=us-east-1

"$@"
