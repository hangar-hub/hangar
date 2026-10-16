"""Start a real Hangar binary for pytest suites.

The launcher mirrors compat/harness in Go: a random port, a temporary SQLite
database and Blob store, and a wait for /readyz. Set HANGAR_BINARY to use a
prebuilt binary; otherwise the launcher runs `go build ./cmd/hangar` once and
removes the binary when the Python process exits.
"""

from __future__ import annotations

import atexit
import os
import shutil
import signal
import socket
import subprocess
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

import requests

REPO_ROOT = Path(__file__).resolve().parent.parent


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


_built: str | None = None


def hangar_binary() -> str:
    global _built
    if os.environ.get("HANGAR_BINARY"):
        return os.environ["HANGAR_BINARY"]
    if _built is None:
        build_dir = tempfile.mkdtemp(prefix="hangar-compat-bin-")
        atexit.register(shutil.rmtree, build_dir, ignore_errors=True)
        out = Path(build_dir) / "hangar"
        subprocess.run(["go", "build", "-o", str(out), "./cmd/hangar"], cwd=REPO_ROOT, check=True)
        _built = str(out)
    return _built


@dataclass
class Instance:
    url: str
    data_dir: Path
    process: subprocess.Popen
    log_path: Path
    extra: dict = field(default_factory=dict)

    def logs(self) -> str:
        return self.log_path.read_text(errors="replace")

    def stop(self, timeout: float = 30) -> None:
        if self.process.poll() is not None:
            return
        self.process.send_signal(signal.SIGTERM)
        try:
            self.process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.process.kill()
            raise RuntimeError("hangar did not stop in time and was killed")


def start(settings: list[str] | None = None, timeout: float = 60) -> Instance:
    port = free_port()
    data_dir = Path(tempfile.mkdtemp(prefix="hangar-compat-"))
    url = f"http://127.0.0.1:{port}"
    args = [
        hangar_binary(), "server",
        "--set", f"server.listen=127.0.0.1:{port}",
        "--set", f"server.public_url={url}",
        "--set", f"server.data_dir={data_dir}",
        "--set", f"database.dsn={data_dir / 'hangar.db'}",
        "--set", f"storage.fs.path={data_dir / 'blobs'}",
        "--set", f"git.cache_dir={data_dir / 'git-cache'}",
    ]
    for s in settings or []:
        args += ["--set", s]
    env = {k: v for k, v in os.environ.items() if not k.startswith("HANGAR_")}
    log_path = data_dir / "hangar.log"
    log = open(log_path, "wb")  # noqa: SIM115 - the process keeps it open
    proc = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, env=env)
    inst = Instance(url=url, data_dir=data_dir, process=proc, log_path=log_path)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"hangar exited with {proc.returncode}:\n{inst.logs()}")
        try:
            if requests.get(f"{url}/readyz", timeout=2).status_code == 200:
                return inst
        except requests.ConnectionError:
            pass
        time.sleep(0.1)
    inst.stop()
    raise RuntimeError(f"hangar did not become ready in {timeout}s:\n{inst.logs()}")


def cleanup(inst: Instance) -> None:
    inst.stop()
    shutil.rmtree(inst.data_dir, ignore_errors=True)
