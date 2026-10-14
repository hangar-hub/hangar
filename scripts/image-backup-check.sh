#!/usr/bin/env bash
# Checks the Docker backup recipe in docs/operate/backup.md: hangar admin
# backup, running as UID 65532 in the image, writes into a new named volume
# mounted at /backups, the backup copies out with docker cp, and it
# restores from that volume. A volume mounted at a path the image does
# not own is created root-owned, and the backup fails with "permission
# denied".
# Usage: scripts/image-backup-check.sh <image>
set -euo pipefail
image="${1:?usage: scripts/image-backup-check.sh <image>}"

id="hangar-backup-check-$$"
data="$id-data"
backups="$id-backups"
tmp="$(mktemp -d)"
cleanup() {
  docker rm -f "$id-copy" >/dev/null 2>&1 || true
  docker volume rm -f "$data" "$backups" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

run() { docker run --rm -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 "$@"; }

run -v "$data:/var/lib/hangar" "$image" admin migrate >/dev/null

# --skip-blobs keeps the check to the database and the volume: what fails
# without a writable /backups is the first mkdir, before any object is read.
if ! run -v "$data:/var/lib/hangar" -v "$backups:/backups" "$image" \
  admin backup --skip-blobs --output /backups/check >"$tmp/backup.log" 2>&1; then
  echo "error: hangar admin backup could not write to a named volume at /backups:" >&2
  cat "$tmp/backup.log" >&2
  exit 1
fi
echo "ok: hangar admin backup writes to a named volume at /backups"

docker create --name "$id-copy" -v "$backups:/backups" "$image" >/dev/null
docker cp "$id-copy:/backups/check" "$tmp/check" >/dev/null
if [[ ! -s "$tmp/check/manifest.json" ]]; then
  echo "error: the copied backup has no manifest.json" >&2
  exit 1
fi
echo "ok: docker cp copies the backup out of the volume"

if ! run -v "$data:/var/lib/hangar" -v "$backups:/backups:ro" "$image" \
  admin restore /backups/check --force --skip-blobs >"$tmp/restore.log" 2>&1; then
  echo "error: hangar admin restore could not read the backup:" >&2
  cat "$tmp/restore.log" >&2
  exit 1
fi
echo "ok: hangar admin restore reads the backup from the volume"
