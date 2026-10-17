#!/usr/bin/env bash
# Installs the Helm chart on a throwaway kind cluster and runs the cluster
# tests in deploy/kind_test.go: generated secrets kept across renders, and a
# GitOps-style install with Postgres and MinIO applied in the same step.
# Deletes the cluster afterwards.
#
# Usage: scripts/kind-chart-test.sh <hangar image>
#   scripts/kind-chart-test.sh hangar:ci
#
# KIND_CLUSTER names the cluster (default hangar-chart); KIND_NODE_IMAGE
# picks the node image. With KIND_CLUSTER_EXISTS=1 the script uses the
# cluster as it is and leaves it running (CI creates its own).
set -euo pipefail
cd "$(dirname "$0")/.."
image="${1:?usage: scripts/kind-chart-test.sh <hangar image>}"
cluster="${KIND_CLUSTER:-hangar-chart}"
postgres_image=postgres:18-alpine
minio_image=pgsty/minio:RELEASE.2026-08-04T00-00-00Z

# A kubeconfig of our own, so the cluster never becomes the current context
# in yours.
export KUBECONFIG
KUBECONFIG="$(mktemp)"
cleanup() {
  [[ "${KIND_CLUSTER_EXISTS:-}" == 1 ]] || kind delete cluster --name "$cluster" --kubeconfig "$KUBECONFIG"
  rm -f "$KUBECONFIG"
}
trap cleanup EXIT
if [[ "${KIND_CLUSTER_EXISTS:-}" != 1 ]]; then
  kind create cluster --name "$cluster" --kubeconfig "$KUBECONFIG" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} --wait 2m
else
  kind get kubeconfig --name "$cluster" > "$KUBECONFIG"
fi

# Load what is already local, so the nodes need not pull it. kind load
# docker-image imports every platform, so it fails for a multi-platform
# image that is only partly local (Docker's containerd image store keeps
# only the platform it pulled); load the node's platform from an archive
# instead. Pulling on the node instead took about 5 minutes for Postgres
# and MinIO, which TestKindGitOpsInstall's rollout waits had to absorb.
node_platform="linux/$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')"
archive_dir="$(mktemp -d)"
trap 'rm -rf "$archive_dir"; cleanup' EXIT
for img in "$image" "$postgres_image" "$minio_image"; do
  docker image inspect "$img" > /dev/null 2>&1 || continue
  kind load docker-image "$img" --name "$cluster" && continue
  archive="$archive_dir/image.tar"
  if docker save --platform "$node_platform" -o "$archive" "$img" &&
    kind load image-archive "$archive" --name "$cluster"; then
    echo "loaded $img for $node_platform from an archive" >&2
  else
    echo "could not load $img; the node pulls it" >&2
  fi
  rm -f "$archive"
done

cd deploy
HANGAR_KIND_IMAGE="$image" go test -count=1 -tags 'helm kind' -run '^TestKind' -timeout 20m -v ./
