"""Fixtures for the Hub client compatibility suite (compat/hf).

The suite talks to one of three Hub APIs:

- HANGAR_HF_INSTANCE=binary: the real `hangar` binary (HANGAR_BINARY, or
  built from ./cmd/hangar), started on a temporary SQLite database (or the
  empty Postgres database in HANGAR_HF_DATABASE_DSN with HANGAR_HF_DB=postgres).
  The suite creates the users, Tokens and Organisation through the Hangar API.
- HANGAR_HF_ENDPOINT: an Instance that is already running, with the Tokens in
  HANGAR_HF_TOKENS (JSON: alice, bob, carol and admin).
- Otherwise, the Go test server in compat/hf/server_test.go, which serves
  internal/hfapi over fakes (or over the real Repo service with
  HANGAR_HF_GO_TAGS=w2real).

huggingface_hub reads HF_ENDPOINT and HF_HOME once, at import, so this
module sets them before any test module imports it.
"""

from __future__ import annotations

import json
import os
import secrets
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

# The default of the Instance setting `lfs.multipart_threshold`.
DEFAULT_MULTIPART_THRESHOLD = 64 << 20

# The `lfs.multipart_threshold` of the binary the suite starts: the smallest
# the setting allows, so uploads reach the multipart path with small files.
BINARY_MULTIPART_THRESHOLD = 5 << 20

# Token scopes, as in the Go test server (internal/hfapi/hfapitest).
TOKEN_SCOPES = {"alice": "admin", "bob": "write", "carol": "read", "admin": "admin"}


@dataclass
class Hub:
    endpoint: str
    tokens: dict[str, str]
    org: str
    backend: str
    # LFS uploads larger than this are multipart (`lfs.multipart_threshold`).
    multipart_threshold: int = DEFAULT_MULTIPART_THRESHOLD
    process: subprocess.Popen | None = None
    workdir: Path | None = None
    # The hangar binary under test, with HANGAR_HF_INSTANCE=binary.
    instance: object | None = None

    def token(self, user: str) -> str:
        return self.tokens[user]


def _start_server() -> Hub:
    workdir = Path(tempfile.mkdtemp(prefix="hangar-hf-compat-"))
    try:
        return _build_and_start(workdir)
    except BaseException:
        # The test binary is tens of megabytes: never leave it behind.
        shutil.rmtree(workdir, ignore_errors=True)
        raise


def _server_env(info_path: Path) -> dict[str, str]:
    env = dict(os.environ, HANGAR_HF_SERVE_INFO=str(info_path))
    # HANGAR_HF_DB=postgres needs dbtest's Postgres: start a container unless
    # a server is given with HANGAR_TEST_POSTGRES_DSN.
    if env.get("HANGAR_HF_DB") == "postgres" and not env.get("HANGAR_TEST_POSTGRES_DSN"):
        env.setdefault("HANGAR_TEST_POSTGRES", "1")
    return env


def _build_and_start(workdir: Path) -> Hub:
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
    log_path = workdir / "server.log"
    with open(log_path, "wb") as log:
        proc = subprocess.Popen(
            [str(binary), "-test.run", "^TestServeHub$", "-test.timeout", "0", "-test.v"],
            cwd=HF_DIR,
            stdin=subprocess.PIPE,
            stdout=log,
            stderr=subprocess.STDOUT,
            env=_server_env(info_path),
        )
    try:
        deadline = time.monotonic() + 120
        while not info_path.exists():
            if proc.poll() is not None:
                raise RuntimeError("the Hub test server exited:\n" + log_path.read_text(errors="replace"))
            if time.monotonic() > deadline:
                raise RuntimeError("the Hub test server did not start in time:\n" + log_path.read_text(errors="replace"))
            time.sleep(0.05)
        info = json.loads(info_path.read_text())
    except BaseException:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
        raise
    return Hub(
        info["endpoint"], info["tokens"], info["org"], info["backend"], info["multipart_threshold"], proc, workdir
    )


def _start_binary() -> Hub:
    """Start the real hangar binary and create the suite's users in it."""
    import hangar_instance

    workdir = Path(tempfile.mkdtemp(prefix="hangar-hf-binary-"))
    password_file = workdir / "admin-password"
    password_file.write_text(secrets.token_urlsafe(24))
    password_file.chmod(0o600)
    settings = [
        # As in the Go test server: anonymous users read public Repos, which
        # the download and error tests rely on.
        "security.anonymous_read=true",
        f"lfs.multipart_threshold={BINARY_MULTIPART_THRESHOLD}",
        f"security.bootstrap_admin.password_file={password_file}",
    ]
    database = "sqlite"
    if os.environ.get("HANGAR_HF_DB") == "postgres":
        dsn = os.environ.get("HANGAR_HF_DATABASE_DSN")
        if not dsn:
            shutil.rmtree(workdir, ignore_errors=True)
            raise RuntimeError("HANGAR_HF_DB=postgres with HANGAR_HF_INSTANCE=binary needs HANGAR_HF_DATABASE_DSN")
        settings += ["database.driver=postgres", f"database.dsn={dsn}"]
        database = "postgres"
    try:
        inst = hangar_instance.start(settings)
    except BaseException:
        shutil.rmtree(workdir, ignore_errors=True)
        raise
    try:
        tokens = _create_users(inst.url, password_file.read_text())
    except BaseException:
        # Not the Instance log: on first start it holds the generated
        # Instance key.
        hangar_instance.cleanup(inst)
        shutil.rmtree(workdir, ignore_errors=True)
        raise
    return Hub(
        inst.url, tokens, ORG, f"hangar binary on {database}", BINARY_MULTIPART_THRESHOLD, workdir=workdir, instance=inst
    )


def _create_users(url: str, admin_password: str) -> dict[str, str]:
    """Create alice, bob and carol, their Tokens and the acme Organisation.

    The bootstrap admin (hangar-admin, since `admin` is a reserved Namespace)
    stands in for the Go test server's `admin` user. Returns the Tokens.
    """
    import requests

    api = f"{url}/api/hangar/v1"

    def check(resp: requests.Response, what: str) -> requests.Response:
        # The message names the step and the status only: never the body,
        # which holds a Token secret on success.
        if not resp.ok:
            raise RuntimeError(f"{what}: HTTP {resp.status_code}")
        return resp

    def sign_in(username: str, password: str) -> requests.Session:
        session = requests.Session()
        resp = check(
            session.post(f"{api}/auth/login", json={"username": username, "password": password}, timeout=30),
            f"sign in as {username}",
        )
        session.headers["X-CSRF-Token"] = resp.json()["csrf_token"]
        return session

    def create_token(session: requests.Session, user: str) -> str:
        resp = session.post(f"{api}/me/tokens", json={"name": "compat-hf", "scope": TOKEN_SCOPES[user]}, timeout=30)
        return check(resp, f"create a Token for {user}").json()["secret"]

    admin = sign_in("hangar-admin", admin_password)
    tokens = {"admin": create_token(admin, "admin")}
    for user in ("alice", "bob", "carol"):
        password = secrets.token_urlsafe(24)
        body = {"username": user, "email": f"{user}@example.com", "password": password}
        check(admin.post(f"{api}/admin/users", json=body, timeout=30), f"create user {user}")
        tokens[user] = create_token(sign_in(user, password), user)

    alice = {"Authorization": f"Bearer {tokens['alice']}"}
    check(requests.post(f"{api}/orgs", json={"name": ORG}, headers=alice, timeout=30), f"create the {ORG} Organisation")
    check(
        requests.put(f"{api}/orgs/{ORG}/members/bob", json={"role": "read"}, headers=alice, timeout=30),
        f"add bob to {ORG}",
    )
    return tokens


def _stop_server(hub: Hub) -> None:
    if hub.instance is not None:
        import hangar_instance

        hangar_instance.cleanup(hub.instance)
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


if os.environ.get("HANGAR_HF_INSTANCE") == "binary":
    HUB = _start_binary()
elif os.environ.get("HANGAR_HF_ENDPOINT"):
    HUB = Hub(
        os.environ["HANGAR_HF_ENDPOINT"].rstrip("/"),
        json.loads(os.environ["HANGAR_HF_TOKENS"]),
        os.environ.get("HANGAR_HF_ORG", ORG),
        "external",
        int(os.environ.get("HANGAR_HF_MULTIPART_THRESHOLD", DEFAULT_MULTIPART_THRESHOLD)),
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


def pytest_report_header(config):
    import huggingface_hub

    try:
        from importlib.metadata import version

        xet = version("hf_xet")
    except Exception:  # noqa: BLE001 - any failure means hf_xet is absent
        xet = "not installed"
    xet_uploads = "on" if os.environ.get("HANGAR_HF_XET_UPLOADS") == "1" else "off (HF_HUB_DISABLE_XET=1)"
    return [
        f"hangar compat/hf: huggingface_hub {huggingface_hub.__version__}, hf_xet {xet}, Xet uploads {xet_uploads}",
        f"hangar compat/hf: backend {HUB.backend}, multipart threshold {HUB.multipart_threshold} bytes",
    ]


@pytest.fixture
def no_xet(monkeypatch):
    """Upload through the LFS path, as with HF_HUB_DISABLE_XET=1.

    Xet uploads need the CAS endpoints (W11's internal/xet), which the Go
    test server does not serve. Against an Instance that serves them, set
    HANGAR_HF_XET_UPLOADS=1 to run the same upload tests through hf_xet, the
    matrix cell 13-testing.md asks for.
    """
    from huggingface_hub import constants

    if os.environ.get("HANGAR_HF_XET_UPLOADS") == "1":
        return
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
        if HUB.process is not None or HUB.instance is not None:
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
        "generated_by": "compat/hf (HANGAR_HF_INSTANCE=binary HANGAR_HF_WRITE_MATRIX=1 uv run --project compat pytest compat/hf)",
        "huggingface_hub": huggingface_hub.__version__,
        "backend": HUB.backend,
        "endpoints": endpoints,
    }
    MATRIX_PATH.write_text(json.dumps(doc, indent=2, sort_keys=True) + "\n")
