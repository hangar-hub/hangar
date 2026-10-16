"""Checks the pytest launcher itself."""

import os
import subprocess
import sys
from pathlib import Path

import requests

import hangar_instance


def test_instance_serves_health(hangar):
    resp = requests.get(f"{hangar.url}/healthz", timeout=5)
    assert resp.status_code == 200
    assert resp.json() == {"status": "ok"}
    assert resp.headers["X-Request-Id"]


def test_instances_are_isolated_and_stop():
    inst = hangar_instance.start()
    try:
        assert requests.get(f"{inst.url}/readyz", timeout=5).status_code == 200
    finally:
        hangar_instance.cleanup(inst)
    assert inst.process.poll() is not None


def test_built_binary_is_removed_when_the_process_exits(tmp_path):
    env = {k: v for k, v in os.environ.items() if k != "HANGAR_BINARY"}
    env["TMPDIR"] = str(tmp_path)
    script = "import hangar_instance; print(hangar_instance.hangar_binary())"
    out = subprocess.run(
        [sys.executable, "-c", script],
        cwd=Path(hangar_instance.__file__).parent,
        env=env,
        capture_output=True,
        text=True,
        check=True,
    )
    built = Path(out.stdout.strip())
    assert built.parent.parent == tmp_path
    assert not built.parent.exists()
    assert list(tmp_path.glob("hangar-compat-bin-*")) == []
