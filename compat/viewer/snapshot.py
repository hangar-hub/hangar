"""Record the Hub's Dataset Viewer responses for the compat/viewer datasets.

Usage: python compat/viewer/snapshot.py [--refresh] [--offline]

For every dataset in datasets.json this writes hub/<namespace>__<name>.json
with the Revision the Hub's viewer answered for (its X-Revision header) and
the responses of /is-valid, /splits, /info, /size, /parquet, and per Split
/first-rows, /rows (two pages) and /statistics. It then downloads the
dataset's files at that Revision into compat/.fixtures/viewer/<namespace>__<name>/,
so Hangar converts exactly what the Hub converted. Existing snapshots are
kept unless --refresh is given; --offline only checks that everything is
cached. Only the Python standard library is used.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
HUB = HERE / "hub"
FIXTURES = HERE.parent / ".fixtures" / "viewer"
VIEWER = "https://datasets-server.huggingface.co"
HUB_URL = "https://huggingface.co"


def get(url: str, attempts: int = 6) -> tuple[int, dict, dict]:
    """GET a JSON URL with retries on 5xx and connection errors."""
    last: Exception | None = None
    for i in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=120) as r:
                return r.status, dict(r.headers), json.loads(r.read())
        except urllib.error.HTTPError as e:
            if e.code < 500:
                return e.code, dict(e.headers), json.loads(e.read() or b"{}")
            last = e
        except (urllib.error.URLError, TimeoutError) as e:
            last = e
        time.sleep(2 * (i + 1))
    raise RuntimeError(f"GET {url} failed: {last}")


def viewer(endpoint: str, **params: str | int) -> tuple[int, str, dict]:
    status, headers, body = get(f"{VIEWER}/{endpoint}?{urllib.parse.urlencode(params)}")
    rev = next((v for k, v in headers.items() if k.lower() == "x-revision"), "")
    return status, rev, body


def snapshot(repo: str) -> dict:
    out: dict = {"dataset": repo, "responses": {}}
    revisions = set()

    def record(key: str, endpoint: str, **params: str | int) -> dict:
        status, rev, body = viewer(endpoint, dataset=repo, **params)
        if status != 200:
            raise RuntimeError(f"{endpoint} {params} for {repo} answered {status}: {body}")
        if rev:
            revisions.add(rev)
        out["responses"][key] = body
        return body

    for ep in ("is-valid", "splits", "info", "size", "parquet"):
        record(ep, ep)
    for s in out["responses"]["splits"]["splits"]:
        c, sp = s["config"], s["split"]
        prefix = f"{c}/{sp}"
        record(f"first-rows {prefix}", "first-rows", config=c, split=sp)
        rows = record(f"rows {prefix} 0", "rows", config=c, split=sp, offset=0, length=100)
        total = rows["num_rows_total"]
        if total > 100:
            mid = total // 2
            record(f"rows {prefix} {mid}", "rows", config=c, split=sp, offset=mid, length=7)
        record(f"statistics {prefix}", "statistics", config=c, split=sp)
    if len(revisions) != 1:
        raise RuntimeError(f"{repo}: the viewer answered for several Revisions {revisions}; run again later")
    out["revision"] = revisions.pop()
    return out


def download(repo: str, revision: str) -> None:
    target = FIXTURES / repo.replace("/", "__")
    marker = target / ".revision"
    if marker.exists() and marker.read_text() == revision:
        return
    _, _, tree = get(f"{HUB_URL}/api/datasets/{repo}/tree/{revision}?recursive=1")
    for entry in tree:
        if entry["type"] != "file":
            continue
        dest = target / entry["path"]
        dest.parent.mkdir(parents=True, exist_ok=True)
        url = f"{HUB_URL}/datasets/{repo}/resolve/{revision}/{urllib.parse.quote(entry['path'])}"
        with urllib.request.urlopen(url, timeout=300) as r:
            dest.write_bytes(r.read())
    marker.write_text(revision)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--refresh", action="store_true", help="record the Hub's responses again")
    parser.add_argument("--offline", action="store_true", help="only check the cache")
    args = parser.parse_args()
    manifest = json.loads((HERE / "datasets.json").read_text())
    HUB.mkdir(exist_ok=True)
    missing = []
    for ds in manifest["datasets"]:
        repo = ds["repo"]
        path = HUB / (repo.replace("/", "__") + ".json")
        if args.offline:
            target = FIXTURES / repo.replace("/", "__") / ".revision"
            if not path.exists() or not target.exists():
                missing.append(repo)
            continue
        if args.refresh or not path.exists():
            snap = snapshot(repo)
            path.write_text(json.dumps(snap, indent=1, ensure_ascii=False, sort_keys=False) + "\n")
            print(f"recorded   {repo} @ {snap['revision'][:8]}")
        snap = json.loads(path.read_text())
        download(repo, snap["revision"])
        print(f"downloaded {repo} @ {snap['revision'][:8]}")
    if missing:
        print("missing: " + ", ".join(missing), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
