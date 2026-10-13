"""Download the upstream fixtures listed in fixtures.json, with caching.

Usage: uv run --project compat python compat/fixtures.py [--nightly] [--offline]

Each fixture goes to compat/.fixtures/<type>s/<namespace>/<name>. A fixture
whose recorded commit still matches upstream is not downloaded again; with
--offline the cache is used as it is. compat/.fixtures/lock.json records the
commit of every fixture, so tests can check Commit SHAs against it.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from huggingface_hub import HfApi, snapshot_download

HERE = Path(__file__).resolve().parent
CACHE = HERE / ".fixtures"
LOCK = CACHE / "lock.json"


def load_lock() -> dict:
    if LOCK.exists():
        return json.loads(LOCK.read_text())
    return {}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--nightly", action="store_true", help="also download nightly-only fixtures")
    parser.add_argument("--offline", action="store_true", help="only check the cache")
    args = parser.parse_args()

    manifest = json.loads((HERE / "fixtures.json").read_text())
    lock = load_lock()
    api = HfApi(endpoint="https://huggingface.co")
    missing = []
    for fx in manifest["fixtures"]:
        if fx.get("nightly") and not args.nightly:
            continue
        key = f"{fx['type']}:{fx['repo']}"
        target = CACHE / f"{fx['type']}s" / fx["repo"]
        if args.offline:
            if not target.exists() or key not in lock:
                missing.append(key)
            continue
        sha = api.repo_info(fx["repo"], repo_type=fx["type"]).sha
        if lock.get(key) == sha and target.exists():
            print(f"cached     {key} @ {sha[:8]}")
            continue
        snapshot_download(
            repo_id=fx["repo"],
            repo_type=fx["type"],
            revision=sha,
            local_dir=target,
            allow_patterns=fx.get("allow_patterns"),
        )
        lock[key] = sha
        print(f"downloaded {key} @ {sha[:8]}")
    if missing:
        print("missing from the cache: " + ", ".join(missing), file=sys.stderr)
        return 1
    CACHE.mkdir(parents=True, exist_ok=True)
    LOCK.write_text(json.dumps(lock, indent=2, sort_keys=True) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
