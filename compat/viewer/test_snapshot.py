"""Checks that snapshot.py's fixture download survives a flaky Hub.

A local server stands in for huggingface.co and answers each request from
a script: stall mid-body, end the body early, send the wrong bytes, answer
503 or 404. The download must retry the transient failures, keep only files
that match the Hub's file list, and fail fast on the permanent ones.
"""

from __future__ import annotations

import hashlib
import json
import threading
import time
from collections import Counter
from collections.abc import Callable, Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

import snapshot

REPO = "ns/demo"
REV = "0123456789abcdef0123456789abcdef01234567"
CSV = b"id,text\n1,hello\n2,world\n"
PARQUET = bytes(range(256)) * 64
TIMEOUT = 0.5

Reply = Callable[[BaseHTTPRequestHandler], None]


def blob_oid(data: bytes) -> str:
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


TREE = [
    {"type": "directory", "oid": "0" * 40, "size": 0, "path": "data"},
    {"type": "file", "oid": blob_oid(CSV), "size": len(CSV), "path": "data/train.csv"},
    {
        "type": "file",
        "oid": "1" * 40,
        "size": len(PARQUET),
        "lfs": {"oid": hashlib.sha256(PARQUET).hexdigest(), "size": len(PARQUET), "pointerSize": 130},
        "path": "data/test.parquet",
    },
]
TREE_PATH = f"/api/datasets/{REPO}/tree/{REV}?recursive=1"


def file_path(name: str) -> str:
    return f"/datasets/{REPO}/resolve/{REV}/{name}"


def body(data: bytes) -> Reply:
    def reply(h: BaseHTTPRequestHandler) -> None:
        h.send_response(200)
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        h.wfile.write(data)

    return reply


def status(code: int) -> Reply:
    def reply(h: BaseHTTPRequestHandler) -> None:
        h.send_response(code)
        h.send_header("Content-Length", "2")
        h.end_headers()
        h.wfile.write(b"{}")

    return reply


def stall(data: bytes) -> Reply:
    """Send the headers and half the body, then go quiet past the client's timeout."""

    def reply(h: BaseHTTPRequestHandler) -> None:
        h.send_response(200)
        h.send_header("Content-Length", str(len(data)))
        h.end_headers()
        h.wfile.write(data[: len(data) // 2])
        h.wfile.flush()
        time.sleep(TIMEOUT * 4)

    return reply


def short(data: bytes) -> Reply:
    """Promise the whole body, send half of it and close the connection."""

    def reply(h: BaseHTTPRequestHandler) -> None:
        h.send_response(200)
        h.send_header("Content-Length", str(len(data)))
        h.send_header("Connection", "close")
        h.end_headers()
        h.wfile.write(data[: len(data) // 2])
        h.close_connection = True

    return reply


class Hub:
    """A scripted stand-in for huggingface.co that counts requests per path."""

    def __init__(self) -> None:
        self.script: dict[str, list[Reply]] = {}
        self.hits: Counter[str] = Counter()
        hub = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                hub.hits[self.path] += 1
                replies = hub.script.get(self.path)
                if not replies:
                    status(404)(self)
                    return
                reply = replies.pop(0) if len(replies) > 1 else replies[0]
                try:
                    reply(self)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *args: object) -> None:
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def hub(monkeypatch) -> Iterator[Hub]:
    monkeypatch.setattr(snapshot, "backoff", lambda attempt: None)
    h = Hub()
    h.script[TREE_PATH] = [body(json.dumps(TREE).encode())]
    yield h
    h.close()


def download(hub: Hub, root: Path) -> Path:
    snapshot.download(REPO, REV, hub_url=hub.url, root=root, timeout=TIMEOUT)
    return root / REPO.replace("/", "__")


def no_part_files(target: Path) -> bool:
    return not list(target.rglob("*.part"))


def test_retries_a_read_that_times_out(hub, tmp_path):
    # The failure seen in compat/viewer: one read from huggingface.co hung
    # until the timeout and the TimeoutError ended the whole run.
    hub.script[file_path("data/train.csv")] = [stall(CSV), body(CSV)]
    hub.script[file_path("data/test.parquet")] = [stall(PARQUET), body(PARQUET)]
    target = download(hub, tmp_path)
    assert (target / "data/train.csv").read_bytes() == CSV
    assert (target / "data/test.parquet").read_bytes() == PARQUET
    assert (target / ".revision").read_text() == REV
    assert hub.hits[file_path("data/train.csv")] == 2
    assert hub.hits[file_path("data/test.parquet")] == 2
    assert no_part_files(target)


def test_retries_short_bodies_wrong_bytes_and_server_errors(hub, tmp_path):
    wrong = bytes(len(PARQUET))
    hub.script[TREE_PATH].insert(0, status(503))
    hub.script[file_path("data/train.csv")] = [status(429), short(CSV), body(CSV)]
    hub.script[file_path("data/test.parquet")] = [status(502), body(wrong), body(PARQUET)]
    target = download(hub, tmp_path)
    assert (target / "data/train.csv").read_bytes() == CSV
    assert (target / "data/test.parquet").read_bytes() == PARQUET
    assert hub.hits[TREE_PATH] == 2
    assert hub.hits[file_path("data/train.csv")] == 3
    assert hub.hits[file_path("data/test.parquet")] == 3
    assert no_part_files(target)


def test_resumes_without_fetching_files_that_already_match(hub, tmp_path):
    target = tmp_path / REPO.replace("/", "__")
    (target / "data").mkdir(parents=True)
    (target / "data/train.csv").write_bytes(CSV)
    (target / "data/test.parquet").write_bytes(PARQUET[:100])
    hub.script[file_path("data/train.csv")] = [body(CSV)]
    hub.script[file_path("data/test.parquet")] = [body(PARQUET)]
    download(hub, tmp_path)
    assert hub.hits[file_path("data/train.csv")] == 0
    assert hub.hits[file_path("data/test.parquet")] == 1
    assert (target / "data/test.parquet").read_bytes() == PARQUET
    assert (target / ".revision").read_text() == REV


def test_fails_fast_on_a_missing_file(hub, tmp_path):
    hub.script[file_path("data/train.csv")] = [body(CSV)]
    with pytest.raises(RuntimeError, match="answered 404"):
        download(hub, tmp_path)
    target = tmp_path / REPO.replace("/", "__")
    assert hub.hits[file_path("data/test.parquet")] == 1
    assert not (target / ".revision").exists()
    assert no_part_files(target)


def test_gives_up_after_the_last_attempt(hub, tmp_path):
    hub.script[file_path("data/train.csv")] = [stall(CSV)]
    with pytest.raises(RuntimeError, match="failed after 6 attempts"):
        download(hub, tmp_path)
    target = tmp_path / REPO.replace("/", "__")
    assert hub.hits[file_path("data/train.csv")] == 6
    assert not (target / ".revision").exists()
    assert not (target / "data/train.csv").exists()
    assert no_part_files(target)


def test_fails_on_a_missing_file_list(hub, tmp_path):
    hub.script[TREE_PATH] = [status(404)]
    with pytest.raises(RuntimeError, match="answered 404"):
        download(hub, tmp_path)
