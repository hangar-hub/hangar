#!/usr/bin/env bash
# Seeds an Instance with the Repos the load tests use:
#   <ns>/resolve       a few files, for resolve.js and model_info.js
#   <ns>/many-files    100,000 small files, for tree.js
#   <ns>/many-commits  10,000 Commits, for git_clone.js
# The Repos are pushed with git, so the Instance needs no Upstream access.
#
# Usage: HANGAR_URL=... HANGAR_TOKEN=... HANGAR_USERNAME=... loadtest/seed.sh
set -euo pipefail
url="${HANGAR_URL:?set HANGAR_URL}"
token="${HANGAR_TOKEN:?set HANGAR_TOKEN (a write Token)}"
user="${HANGAR_USERNAME:?set HANGAR_USERNAME}"
ns="${HANGAR_NAMESPACE:-$user}"
files="${TREE_FILES:-100000}"
commits="${CLONE_COMMITS:-10000}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

create() {
  curl -fsS -o /dev/null -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    --data "{\"type\":\"model\",\"namespace\":\"$ns\",\"name\":\"$1\"}" "$url/api/hangar/v1/repos" || true
}
remote() {
  local proto="${url%%://*}" rest="${url#*://}"
  echo "$proto://$user:$token@$rest/$ns/$1.git"
}

echo "Seeding $ns/resolve"
create resolve
git init -q -b main "$work/resolve"
( cd "$work/resolve"
  git lfs install --local >/dev/null
  git lfs track '*.safetensors' >/dev/null
  echo '{"model_type": "bert"}' > config.json
  head -c 2000000 /dev/urandom | base64 > tokenizer.json
  head -c 50000000 /dev/urandom > model.safetensors
  echo '# Load test model' > README.md
  git add -A && git -c user.name=loadtest -c user.email=loadtest@example.com commit -qm "Seed"
  git push -q --force "$(remote resolve)" main )

echo "Seeding $ns/many-files ($files files)"
create many-files
git init -q -b main "$work/many-files"
( cd "$work/many-files"
  for ((d = 0; d < files / 1000; d++)); do
    mkdir -p "d$d"
    for ((f = 0; f < 1000; f++)); do echo "$d/$f" > "d$d/f$f.txt"; done
  done
  git add -A && git -c user.name=loadtest -c user.email=loadtest@example.com commit -qm "Seed"
  git push -q --force "$(remote many-files)" main )

echo "Seeding $ns/many-commits ($commits Commits)"
create many-commits
git init -q -b main "$work/many-commits"
( cd "$work/many-commits"
  {
    for ((i = 1; i <= commits; i++)); do
      printf 'commit refs/heads/main\ncommitter loadtest <loadtest@example.com> %d +0000\ndata <<EOF\nCommit %d\nEOF\n' "$((1700000000 + i))" "$i"
      printf 'M 644 inline file-%d.txt\ndata <<EOF\nline %d\nEOF\n\n' "$((i % 500))" "$i"
    done
  } | git fast-import --quiet
  git push -q --force "$(remote many-commits)" main )

echo "Seeded. Run the scripts with HANGAR_NAMESPACE=$ns."
