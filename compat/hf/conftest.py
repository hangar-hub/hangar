"""Fixtures for the Hub client compatibility suite (compat/hf).

The suite talks to a Hub API at HANGAR_HF_ENDPOINT with the Tokens in
HANGAR_HF_TOKENS (JSON: alice, bob, carol and admin). Without them it builds
and starts the Go test server in compat/hf/server_test.go, which serves
internal/hfapi over fakes (or over the real Repo service with
HANGAR_HF_GO_TAGS=w2real).

huggingface_hub reads HF_ENDPOINT and HF_HOME once, at import, so this
module sets them before any test module imports it.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
HF_DIR = Path(__file__).resolve().parent
MATRIX_PATH = HF_DIR / "compatibility-matrix.json"

# Users and their roles, the same in the Go test server and on a real Instance:
# alice (admin Token) administers the `acme` Organisation, bob (write Token)
# is a read Member of it, carol has a read Token, admin is an Instance admin.
ORG = "acme"


@dataclass
class Hub:
    endpoint: str
    tokens: dict[str, str]
    org: str
    backend: str
    process: subprocess.Popen | None = None
    workdir: Path | None = None

    def token(self, user: str) -> str:
        return self.tokens[user]


def _start_server() -> Hub:
    workdir = Path(tempfile.mkdtemp(prefix="hangar-hf-compat-"))
    binary = workdir / "hfserver.test"
    tags = "compat"
    if os.environ.get("HANGAR_HF_GO_TAGS"):
        tags += "," + os.environ["HANGAR_HF_GO_TAGS"]
    subprocess.run(
        ["go", "test", "-c", "-tags", tags, "-o", str(binary), "./compat/hf"],
        cwd=REPO_ROOT,
        check=True,
    )
    info_path = workdir / "info.json"
    env = dict(os.environ, HANGAR_HF_SERVE_INFO=str(info_path))
    log = open(workdir / "server.log", "wb")  # noqa: SIM115 - the process keeps it open
    proc = subprocess.Popen(
        [str(binary), "-test.run", "^TestServeHub$", "-test.timeout", "0", "-test.v"],
        cwd=HF_DIR,
        stdin=subprocess.PIPE,
        stdout=log,
        stderr=subprocess.STDOUT,
        env=env,
    )
    deadline = time.monotonic() + 120
    while not info_path.exists():
        if proc.poll() is not None:
            raise RuntimeError("the Hub test server exited:\n" + (workdir / "server.log").read_text())
        if time.monotonic() > deadline:
            proc.kill()
            raise RuntimeError("the Hub test server did not start in time")
        time.sleep(0.05)
    info = json.loads(info_path.read_text())
    return Hub(info["endpoint"], info["tokens"], info["org"], info["backend"], proc, workdir)


def _stop_server(hub: Hub) -> None:
    if hub.process is not None:
        assert hub.process.stdin is not None
        hub.process.stdin.close()
        try:
            hub.process.wait(timeout=60)
        except subprocess.TimeoutExpired:
            hub.process.kill()
            raise
    if hub.workdir is not None:
        shutil.rmtree(hub.workdir, ignore_errors=True)


if os.environ.get("HANGAR_HF_ENDPOINT"):
    HUB = Hub(
        os.environ["HANGAR_HF_ENDPOINT"].rstrip("/"),
        json.loads(os.environ["HANGAR_HF_TOKENS"]),
        os.environ.get("HANGAR_HF_ORG", ORG),
        "external",
    )
else:
    HUB = _start_server()

_HF_HOME = tempfile.mkdtemp(prefix="hangar-hf-home-")
os.environ["HF_ENDPOINT"] = HUB.endpoint
os.environ["HF_HOME"] = _HF_HOME
os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"
os.environ["HF_HUB_DISABLE_IMPLICIT_TOKEN"] = "1"
os.environ["HF_UPDATE_DOWNLOAD_COUNTS"] = "0"
os.environ["TOKENIZERS_PARALLELISM"] = "false"
os.environ.pop("HF_TOKEN", None)
os.environ.pop("DISABLE_SAFETENSORS_CONVERSION", None)


def pytest_configure(config):
    config.addinivalue_line("markers", "endpoint(*names): Hub API endpoints a test exercises, for the compatibility matrix")


@pytest.fixture(scope="session")
def hub() -> Hub:
    return HUB


@pytest.fixture
def no_xet(monkeypatch):
    """Upload through the LFS path, as with HF_HUB_DISABLE_XET=1.

    Xet uploads need the CAS endpoints, which compat/xet covers.
    """
    from huggingface_hub import constants

    monkeypatch.setattr(constants, "HF_HUB_DISABLE_XET", True)


@pytest.fixture
def api(hub, no_xet):
    """An HfApi signed in as alice."""
    from huggingface_hub import HfApi

    return HfApi(endpoint=hub.endpoint, token=hub.token("alice"))


@pytest.fixture
def repo_name(request) -> str:
    """A Repo name unique to the test."""
    name = request.node.name.replace("[", "-").replace("]", "").replace("_", "-")
    return f"{name[:60]}-{int(time.time() * 1000) % 100000}"


# Compatibility matrix ------------------------------------------------------

_results: dict[str, list[tuple[str, str]]] = {}


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    report = outcome.get_result()
    if report.when != "call" and not (report.when == "setup" and report.outcome != "passed"):
        return
    for mark in item.iter_markers(name="endpoint"):
        for ep in mark.args:
            _results.setdefault(ep, []).append((item.nodeid.split("::")[-1], report.outcome))


def pytest_sessionfinish(session, exitstatus):
    try:
        if _results and os.environ.get("HANGAR_HF_WRITE_MATRIX") == "1":
            _write_matrix()
    finally:
        if HUB.process is not None:
            _stop_server(HUB)
        shutil.rmtree(_HF_HOME, ignore_errors=True)


# Endpoints Hangar does not implement in v1.0 (docs/spec/03-hf-api.md "Scope").
NOT_SUPPORTED = {
    "GET /api/{type}s/{repo}/xet-read-token/{rev}": "Hangar never offers Xet downloads; downloads use plain HTTP.",
    "POST /api/{type}s/{repo}/lfs-files/duplicate": "Cross-Repo LFS copy needs Xet hashes. Copies within a Repo work.",
    "/api/spaces/...": "Spaces are out of scope.",
    "/api/jobs/... and /api/inference/...": "Jobs and the Inference API are out of scope.",
    "POST /api/{type}s/{repo}/discussions/{num}/comment/{id}/edit|hide": "Editing and hiding comments are not in v1.0.",
    "GET /api/users/{user}/likes, GET /api/{type}s/{repo}/likers": "Liker listings are not in v1.0; like and unlike work.",
}


def _write_matrix():
    import huggingface_hub

    endpoints = {}
    for ep, runs in sorted(_results.items()):
        outcomes = {o for _, o in runs}
        status = "supported" if outcomes == {"passed"} else ("not supported" if "passed" not in outcomes else "partial")
        endpoints[ep] = {"status": status, "tests": sorted({n for n, _ in runs})}
    for ep, note in NOT_SUPPORTED.items():
        endpoints[ep] = {"status": "not supported", "note": note}
    doc = {
        "generated_by": "compat/hf (HANGAR_HF_WRITE_MATRIX=1 uv run --project compat pytest compat/hf)",
        "huggingface_hub": huggingface_hub.__version__,
        "backend": HUB.backend,
        "endpoints": endpoints,
    }
    MATRIX_PATH.write_text(json.dumps(doc, indent=2, sort_keys=True) + "\n")
