#!/usr/bin/env bash
# Rehearses the signing and SBOM steps of a release (OPS-7) on this machine,
# with a throwaway cosign key in place of the release workflow's GitHub OIDC
# identity and no transparency log, so nothing leaves the machine.
#
#   binaries  Runs GoReleaser on .goreleaser.yaml (snapshot, no publish) with
#             its sboms and signs blocks, the signs block given the throwaway
#             key, then verifies the checksums.txt bundle, the archives
#             against checksums.txt and an SPDX SBOM per archive.
#   image     Pushes IMAGE to a throwaway local registry and runs the
#             cosign sign, syft SBOM and cosign attest steps of release.yml,
#             then verifies the signature and the SBOM attestation.
#
# Needs cosign 3 (what sigstore/cosign-installer installs), syft, jq, and
# GoReleaser v2 and yq for binaries or Docker and Go (for crane) for image.
# Usage: scripts/release-rehearsal.sh binaries [goreleaser flags...]
#        scripts/release-rehearsal.sh image <image>
set -euo pipefail
cd "$(dirname "$0")/.."

CRANE="${CRANE:-go run github.com/google/go-containerregistry/cmd/crane@v0.22.1}"

mode="${1:?usage: scripts/release-rehearsal.sh binaries|image ...}"
shift

need() {
  for tool in "$@"; do
    if ! command -v "$tool" >/dev/null; then
      echo "error: $tool is not on PATH" >&2
      exit 1
    fi
  done
}
need cosign syft jq
if ! cosign version 2>/dev/null | grep -Eq 'GitVersion:[[:space:]]+v3\.'; then
  echo "error: needs cosign 3, as sigstore/cosign-installer installs; found: $(cosign version 2>/dev/null | grep GitVersion)" >&2
  exit 1
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/hangar-rehearsal.XXXXXX")"
registry=""
cleanup() {
  if [ -n "$registry" ]; then docker rm -f "$registry" >/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

# A throwaway key pair, and a signing config with no certificate authority,
# transparency log or timestamp authority, so signing stays offline.
export COSIGN_PASSWORD=""
(cd "$work" && cosign generate-key-pair >/dev/null)
# A second key, which must not verify anything the first one signed.
mkdir "$work/other"
(cd "$work/other" && cosign generate-key-pair >/dev/null)
cat >"$work/signing-config.json" <<'EOF'
{"mediaType": "application/vnd.dev.sigstore.signingconfig.v0.2+json",
 "caUrls": [], "oidcUrls": [], "rekorTlogUrls": [], "tsaUrls": [],
 "rekorTlogConfig": {"selector": "ANY"}, "tsaConfig": {"selector": "ANY"}}
EOF
sign_flags=(--key "$work/cosign.key" --signing-config "$work/signing-config.json")
verify_flags=(--key "$work/cosign.pub" --insecure-ignore-tlog=true)

# check_sbom fails unless the SPDX document lists the hangar Go module.
check_sbom() {
  local doc="$1" what="$2"
  if ! jq -e '.spdxVersion and ([.packages[]? | select(.name == "github.com/jamesagarside/hangar")] | length > 0)' \
    "$doc" >/dev/null; then
    echo "error: the SBOM of $what is not SPDX or does not list github.com/jamesagarside/hangar" >&2
    exit 1
  fi
  echo "ok: SPDX SBOM of $what lists $(jq '.packages | length' "$doc") packages, github.com/jamesagarside/hangar among them"
}

rehearse_binaries() {
  need goreleaser yq
  # The release's own configuration, with the throwaway key added to the
  # signs block. Everything else is unchanged.
  yq '.signs[0].args += ["--key='"$work"'/cosign.key", "--signing-config='"$work"'/signing-config.json"]' \
    .goreleaser.yaml >"$work/goreleaser.yaml"
  # The Homebrew cask is rendered but not pushed (--skip=publish); its token
  # template needs a value.
  HOMEBREW_TAP_TOKEN="${HOMEBREW_TAP_TOKEN:-unused-without-publish}" \
    goreleaser release --snapshot --clean --skip=publish --config "$work/goreleaser.yaml" "$@"

  local bundle=dist/checksums.txt.sigstore.json
  if [ ! -s "$bundle" ]; then
    echo "error: GoReleaser wrote no $bundle" >&2
    exit 1
  fi
  cosign verify-blob dist/checksums.txt --bundle "$bundle" "${verify_flags[@]}"
  echo "ok: checksums.txt verifies against its Sigstore bundle"
  cp dist/checksums.txt "$work/tampered.txt"
  echo "0000  extra" >>"$work/tampered.txt"
  if cosign verify-blob "$work/tampered.txt" --bundle "$bundle" "${verify_flags[@]}" 2>/dev/null; then
    echo "error: the bundle verified a changed checksums.txt" >&2
    exit 1
  fi
  echo "ok: a changed checksums.txt does not verify"
  (cd dist && shasum -a 256 --ignore-missing -c checksums.txt)

  local archives=0 archive
  for archive in dist/hangar_*.tar.gz dist/hangar_*.zip; do
    [ -e "$archive" ] || continue
    archives=$((archives + 1))
    if ! grep -q " $(basename "$archive")\$" dist/checksums.txt; then
      echo "error: checksums.txt does not list $archive" >&2
      exit 1
    fi
    check_sbom "$archive.spdx.json" "$(basename "$archive")"
  done
  if [ "$archives" -eq 0 ]; then
    echo "error: GoReleaser wrote no archives" >&2
    exit 1
  fi
}

rehearse_image() {
  local image="${1:?usage: scripts/release-rehearsal.sh image <image>}"
  need docker go
  registry="hangar-rehearsal-registry-$$"
  docker run -d --rm --name "$registry" -p 127.0.0.1::5000 registry:2.8.3 >/dev/null
  local port
  port="$(docker port "$registry" 5000/tcp | head -1 | sed 's/.*://')"
  # Docker Desktop pushes from its VM, where the published port is not on
  # localhost, so crane pushes the saved image from this host instead.
  docker save "$image" -o "$work/image.tar"
  local ref
  local crane
  read -r -a crane <<<"$CRANE"
  ref="$("${crane[@]}" push --insecure "$work/image.tar" "127.0.0.1:$port/hangar:rehearsal")"
  rm -f "$work/image.tar"
  case "$ref" in
    127.0.0.1:"$port"/hangar@sha256:*) ;;
    *)
      echo "error: crane push printed no digest reference: $ref" >&2
      exit 1
      ;;
  esac

  # The steps of the image job in .github/workflows/release.yml.
  cosign sign --yes "${sign_flags[@]}" "$ref"
  cosign verify "${verify_flags[@]}" "$ref" >/dev/null
  echo "ok: the image signature verifies"
  if cosign verify --key "$work/other/cosign.pub" --insecure-ignore-tlog=true "$ref" >/dev/null 2>&1; then
    echo "error: the image verified against a key that did not sign it" >&2
    exit 1
  fi
  echo "ok: the image does not verify against another key"
  SYFT_REGISTRY_INSECURE_USE_HTTP=true syft scan -q "registry:$ref" -o "spdx-json=$work/sbom.spdx.json"
  check_sbom "$work/sbom.spdx.json" "the image"
  cosign attest --yes "${sign_flags[@]}" --type spdxjson --predicate "$work/sbom.spdx.json" "$ref"
  cosign verify-attestation "${verify_flags[@]}" --type spdxjson "$ref" >"$work/attestation.json"
  if ! jq -e -s '[.[] | .payload | @base64d | fromjson | .predicateType] | index("https://spdx.dev/Document") != null' \
    "$work/attestation.json" >/dev/null; then
    echo "error: the verified attestation is not an SPDX document" >&2
    exit 1
  fi
  echo "ok: the SPDX SBOM attestation verifies"
}

case "$mode" in
  binaries) rehearse_binaries "$@" ;;
  image) rehearse_image "$@" ;;
  *)
    echo "usage: scripts/release-rehearsal.sh binaries|image ..." >&2
    exit 2
    ;;
esac
