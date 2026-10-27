#!/usr/bin/env bash
# Writes the hangar shell completion scripts to completions/: hangar.bash,
# hangar.zsh, hangar.fish and hangar.ps1. GoReleaser runs it before the
# builds and puts them in the release archives and the Homebrew cask
# (.goreleaser.yaml).
# Usage: scripts/completions.sh
set -euo pipefail
cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
go build -o "$tmp/hangar" ./cmd/hangar

rm -rf completions
mkdir completions
for sh in bash zsh fish; do
  "$tmp/hangar" completion "$sh" >"completions/hangar.$sh"
done
"$tmp/hangar" completion powershell >completions/hangar.ps1
