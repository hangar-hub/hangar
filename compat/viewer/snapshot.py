"""Record the Hub's Dataset Viewer responses for the compat/viewer datasets.

Usage: python compat/viewer/snapshot.py [--refresh] [--offline]

For every dataset in datasets.json this writes hub/<namespace>__<name>.json
with the Revision the Hub's viewer answered for (its X-Revision header) and
the responses of /is-valid, /splits, /info, /size, /parquet, and per Split
/first-rows, /rows (two pages) and /statistics. It then downloads the
dataset's files at that Revision into compat/.fixtures/viewer/<namespace>__<name>/,
so Hangar converts exactly what the Hub converted. Each file is retried on
network errors and checked against the size and hash in the Hub's file
list, and files that already match are kept, so a run that fails part way
resumes. Existing snapshots are kept unless --refresh is given; --offline
only checks that everything is cached. Only the Python standard library is
used.
"""

from __future__ import annotations

import argparse
import hashlib
import http.client
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


# Errors worth another attempt: the connection failed or dropped, a read
# timed out, or the body ended early. urlopen wraps only the errors raised
# while connecting in URLError; those raised while reading the body arrive
# as they are.
TRANSIENT = (urllib.error.URLError, TimeoutError, ConnectionError, http.client.HTTPException)


def backoff(attempt: int) -> None:
    """Wait before another attempt; the tests replace it."""
    time.sleep(2 * (attempt + 1))


def retryable(e: urllib.error.HTTPError) -> bool:
    return e.code >= 500 or e.code == 429


def get(url: str, attempts: int = 6, timeout: float = 120) -> tuple[int, dict, dict]:
    """GET a JSON URL with retries on 429, 5xx and transient network errors."""
    last: Exception | None = None
    for i in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=timeout) as r:
                return r.status, dict(r.headers), json.loads(r.read())
        except urllib.error.HTTPError as e:
            if not retryable(e):
                return e.code, dict(e.headers), json.loads(e.read() or b"{}")
            last = e
        except TRANSIENT as e:
            last = e
        if i + 1 < attempts:
            backoff(i)
    raise RuntimeError(f"GET {url} failed after {attempts} attempts: {last!r}")


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


def check(entry: dict, data_path: Path) -> str | None:
    """Return why the file at data_path does not match its tree entry, or None.

    The tree gives every file's size; an LFS file's content hash is the
    SHA-256 in lfs.oid and any other file's is its Git blob ID in oid.
    """
    size = data_path.stat().st_size
    if size != entry["size"]:
        return f"{size} bytes, expected {entry['size']}"
    lfs = entry.get("lfs")
    if lfs:
        h, want = hashlib.sha256(), lfs["oid"]
    else:
        h, want = hashlib.sha1(), entry["oid"]
        h.update(b"blob %d\0" % size)
    with data_path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    if h.hexdigest() != want:
        return f"hash {h.hexdigest()}, expected {want}"
    return None


def fetch_file(url: str, entry: dict, dest: Path, attempts: int = 6, timeout: float = 60) -> None:
    """Download one file to dest, retrying until it matches its tree entry.

    The body goes to a .part file next to dest and replaces dest only once
    its size and hash match, so an interrupted run never leaves a short file.
    timeout applies to each socket operation, so a stalled read fails after
    timeout seconds and is retried rather than ending the run.
    """
    part = dest.with_name(dest.name + ".part")
    last: Exception | str | None = None
    for i in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=timeout) as r, part.open("wb") as out:
                for chunk in iter(lambda: r.read(1 << 20), b""):
                    out.write(chunk)
            last = check(entry, part)
            if last is None:
                part.replace(dest)
                return
        except urllib.error.HTTPError as e:
            if not retryable(e):
                part.unlink(missing_ok=True)
                raise RuntimeError(f"GET {url} answered {e.code}") from e
            last = e
        except TRANSIENT as e:
            last = e
        if i + 1 < attempts:
            backoff(i)
    part.unlink(missing_ok=True)
    raise RuntimeError(f"GET {url} failed after {attempts} attempts: {last!r}")


def download(repo: str, revision: str, hub_url: str = HUB_URL, root: Path = FIXTURES, timeout: float = 60) -> None:
    """Download repo's files at revision into root, keeping files that already match.

    timeout is the limit on each socket operation of a file download.
    """
    target = root / repo.replace("/", "__")
    marker = target / ".revision"
    if marker.exists() and marker.read_text() == revision:
        return
    status, _, tree = get(f"{hub_url}/api/datasets/{repo}/tree/{revision}?recursive=1")
    if status != 200:
        raise RuntimeError(f"the file list of {repo} @ {revision} answered {status}: {tree}")
    for entry in tree:
        if entry["type"] != "file":
            continue
        dest = target / entry["path"]
        if dest.is_file() and check(entry, dest) is None:
            continue
        dest.parent.mkdir(parents=True, exist_ok=True)
        url = f"{hub_url}/datasets/{repo}/resolve/{revision}/{urllib.parse.quote(entry['path'])}"
        fetch_file(url, entry, dest, timeout=timeout)
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
