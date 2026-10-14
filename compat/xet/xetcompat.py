"""Helpers for the Xet upload compatibility suite (compat/xet).

The suite drives real huggingface_hub and hf_xet clients, each in its own
process, against the Go test server in compat/xet/server_test.go: the Hub
API from internal/hfapi with the real Xet Ingestor and CAS handler from
internal/xet in front. Building that server needs internal/hfapi (W4), so
run the suite on a branch that has it. HANGAR_XET_GO_TAGS adds build tags,
such as xetreal for W2's Repo service and W1's Blob store.

Test-only hooks on the server (/_test/stats and /_test/hold-shards) let tests
count CAS calls and hold shard uploads.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

import pytest
import requests

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
XET_DIR = Path(__file__).resolve().parent


@dataclass
class Server:
    endpoint: str
    tokens: dict[str, str]
    org: str
    backend: str
    process: subprocess.Popen
    workdir: Path
    log_path: Path

    def stats(self) -> dict[str, int]:
        r = requests.get(self.endpoint + "/_test/stats", timeout=10)
        r.raise_for_status()
        return r.json()

    def hold_shards(self, on: bool) -> None:
        r = requests.post(self.endpoint + "/_test/hold-shards", params={"on": "1" if on else "0"}, timeout=10)
        r.raise_for_status()

    def log(self) -> str:
        return self.log_path.read_text(errors="replace")


@dataclass
class Builder:
    workdir: Path
    binary: Path | None = None
    servers: list[Server] = field(default_factory=list)

    def build(self) -> Path:
        if self.binary is None:
            if not (REPO_ROOT / "internal" / "hfapi" / "hfapitest").is_dir():
                pytest.fail(
                    "compat/xet needs internal/hfapi/hfapitest (W4). Run it on a branch that merges "
                    "ws/w4-hub-api, or on the integrated main branch."
                )
            tags = "compat,xetserver"
            if os.environ.get("HANGAR_XET_GO_TAGS"):
                tags += "," + os.environ["HANGAR_XET_GO_TAGS"]
            binary = self.workdir / "xetserver.test"
            subprocess.run(
                ["go", "test", "-c", "-tags", tags, "-o", str(binary), "./compat/xet"], cwd=REPO_ROOT, check=True
            )
            self.binary = binary
        return self.binary

    def start(self, **env: str) -> Server:
        binary = self.build()
        workdir = Path(tempfile.mkdtemp(prefix="hangar-xet-server-", dir=self.workdir))
        info_path = workdir / "info.json"
        log_path = workdir / "server.log"
        penv = dict(os.environ, HANGAR_XET_SERVE_INFO=str(info_path), **env)
        if penv.get("HANGAR_XET_DB") == "postgres" and not penv.get("HANGAR_TEST_POSTGRES_DSN"):
            penv.setdefault("HANGAR_TEST_POSTGRES", "1")
        with open(log_path, "wb") as log:
            proc = subprocess.Popen(
                [str(binary), "-test.run", "^TestServeXet$", "-test.timeout", "0", "-test.v"],
                cwd=XET_DIR,
                stdin=subprocess.PIPE,
                stdout=log,
                stderr=subprocess.STDOUT,
                env=penv,
            )
        deadline = time.monotonic() + 120
        while not info_path.exists():
            if proc.poll() is not None:
                raise RuntimeError("the Xet test server exited:\n" + log_path.read_text(errors="replace"))
            if time.monotonic() > deadline:
                proc.kill()
                raise RuntimeError("the Xet test server did not start:\n" + log_path.read_text(errors="replace"))
            time.sleep(0.05)
        info = json.loads(info_path.read_text())
        srv = Server(info["endpoint"], info["tokens"], info["org"], info["backend"], proc, workdir, log_path)
        self.servers.append(srv)
        return srv

    def stop(self, srv: Server) -> None:
        if srv.process.poll() is None:
            assert srv.process.stdin is not None
            srv.process.stdin.close()
            try:
                srv.process.wait(timeout=60)
            except subprocess.TimeoutExpired:
                srv.process.kill()
                srv.process.wait()
        shutil.rmtree(srv.workdir, ignore_errors=True)


def write_random(path: Path, size: int, seed: int) -> str:
    """Writes size bytes that do not compress and returns their SHA-256."""
    path.parent.mkdir(parents=True, exist_ok=True)
    h = hashlib.sha256()
    rng = hashlib.sha256(str(seed).encode()).digest()
    with open(path, "wb") as f:
        left = size
        counter = 0
        while left:
            block = b"".join(
                hashlib.sha256(rng + (counter * 64 + i).to_bytes(8, "little")).digest() for i in range(2048)
            )[:left]
            counter += 1
            f.write(block)
            h.update(block)
            left -= len(block)
    return h.hexdigest()


def write_text(path: Path, size: int, seed: int) -> str:
    """Writes size bytes of compressible text and returns their SHA-256."""
    path.parent.mkdir(parents=True, exist_ok=True)
    lines = []
    total = 0
    i = 0
    while total < size:
        line = f"{i * seed},hangar,model-{i % 97},q4_0,{(i * 7919) % 100003}\n"
        lines.append(line)
        total += len(line)
        i += 1
    data = "".join(lines).encode()[:size]
    path.write_bytes(data)
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


@dataclass
class Client:
    """Runs huggingface_hub in a separate process with its own HF_HOME, so
    hf_xet's cache (keyed by casUrl) belongs to this client alone."""

    server: Server
    home: Path
    user: str = "alice"
    env: dict[str, str] = field(default_factory=dict)

    def environ(self) -> dict[str, str]:
        e = {k: v for k, v in os.environ.items() if not k.startswith(("HF_", "HUGGINGFACE_"))}
        e.update(
            HF_ENDPOINT=self.server.endpoint,
            HF_TOKEN=self.server.tokens[self.user],
            HF_HOME=str(self.home),
            HF_HUB_DISABLE_TELEMETRY="1",
            HF_HUB_DISABLE_PROGRESS_BARS="1",
            HF_XET_LOG_FILE=str(self.home / "xet.log"),
        )
        e.update(self.env)
        return e

    def script(self, code: str) -> list[str]:
        return [sys.executable, "-c", code]

    def run(self, code: str, timeout: float = 600, check: bool = True) -> subprocess.CompletedProcess:
        p = subprocess.run(self.script(code), env=self.environ(), capture_output=True, text=True, timeout=timeout)
        if check and p.returncode != 0:
            raise AssertionError(f"client failed ({p.returncode}):\n{p.stdout}\n{p.stderr}\n--- server log ---\n"
                                 + self.server.log()[-4000:])
        return p

    def start(self, code: str) -> subprocess.Popen:
        return subprocess.Popen(
            self.script(code), env=self.environ(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            start_new_session=True,
        )

    @staticmethod
    def kill(p: subprocess.Popen) -> None:
        os.killpg(p.pid, signal.SIGKILL)
        p.wait()


def hf_xet_version() -> str:
    from importlib.metadata import version

    return version("hf-xet")


