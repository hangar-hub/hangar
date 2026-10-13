"""The `hf` CLI (login, whoami, download, upload), whoami-v2 and the Xet
token endpoint."""

import os
import subprocess
import sys
import time
from pathlib import Path

import pytest
import requests
from huggingface_hub import HfApi

HF = str(Path(sys.executable).parent / "hf")


def run_hf(hub, home, *args, token=None, check=True):
    env = dict(os.environ, HF_ENDPOINT=hub.endpoint, HF_HOME=str(home), HF_HUB_DISABLE_XET="1")
    env.pop("HF_HUB_DISABLE_IMPLICIT_TOKEN", None)
    if token:
        env["HF_TOKEN"] = token
    out = subprocess.run([HF, *args], env=env, capture_output=True, text=True, timeout=120)
    if check:
        assert out.returncode == 0, out.stdout + out.stderr
    return out


@pytest.mark.endpoint("GET /api/whoami-v2")
def test_whoami(hub):
    who = HfApi(endpoint=hub.endpoint, token=hub.token("alice")).whoami()
    assert who["name"] == "alice"
    assert hub.org in [o["name"] for o in who["orgs"]]
    assert who["auth"]["accessToken"]["role"]


@pytest.mark.endpoint("GET /api/whoami-v2 (hf auth)")
def test_hf_auth_login_and_whoami(hub, tmp_path):
    out = run_hf(hub, tmp_path, "auth", "login", "--token", hub.token("bob"))
    assert "Login successful" in out.stdout + out.stderr
    assert (tmp_path / "token").read_text().strip() == hub.token("bob")
    out = run_hf(hub, tmp_path, "auth", "whoami")
    assert "bob" in out.stdout
    bad = run_hf(hub, tmp_path / "other", "auth", "login", "--token", "hf_wrong", check=False)
    assert bad.returncode != 0


@pytest.mark.endpoint("hf download", "hf upload")
def test_hf_download_and_upload(hub, tmp_path):
    repo = f"alice/cli-{int(time.time() * 1000) % 100000}"
    src = tmp_path / "src"
    src.mkdir()
    (src / "config.json").write_text('{"model_type": "bert"}')
    (src / "weights.safetensors").write_bytes(os.urandom(4096))
    run_hf(hub, tmp_path / "home", "repos", "create", repo, token=hub.token("alice"))
    run_hf(hub, tmp_path / "home", "upload", repo, str(src), ".", "--commit-message", "CLI upload", token=hub.token("alice"))
    run_hf(hub, tmp_path / "home", "upload", repo, str(src / "config.json"), "copy/config.json", "--revision", "dev", token=hub.token("alice"))
    dest = tmp_path / "dest"
    run_hf(hub, tmp_path / "home", "download", repo, "--local-dir", str(dest), token=hub.token("alice"))
    assert (dest / "weights.safetensors").read_bytes() == (src / "weights.safetensors").read_bytes()
    out = run_hf(hub, tmp_path / "home", "download", repo, "copy/config.json", "--revision", "dev", token=hub.token("alice"))
    printed = out.stdout.strip().splitlines()[-1].strip().removeprefix("path:").strip()
    assert Path(printed).read_text() == '{"model_type": "bert"}'


@pytest.mark.endpoint("GET /api/{type}s/{repo}/xet-write-token/{rev}")
def test_xet_write_token(hub, api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    r = requests.get(f"{hub.endpoint}/api/models/{repo}/xet-write-token/main", headers={"authorization": f"Bearer {hub.token('alice')}"}, timeout=10)
    assert r.status_code == 200
    body = r.json()
    assert set(body) == {"accessToken", "exp", "casUrl"}
    assert r.headers["X-Xet-Cas-Url"] == body["casUrl"]
    assert r.headers["X-Xet-Access-Token"] == body["accessToken"]
    assert int(r.headers["X-Xet-Token-Expiration"]) == body["exp"]
    assert body["exp"] > time.time()
    # huggingface_hub's refresh URL for create_pr ends in /None.
    r = requests.get(f"{hub.endpoint}/api/models/{repo}/xet-write-token/None?create_pr=1", headers={"authorization": f"Bearer {hub.token('bob')}"}, timeout=10)
    assert r.status_code == 200
    r = requests.get(f"{hub.endpoint}/api/models/{repo}/xet-read-token/main", headers={"authorization": f"Bearer {hub.token('alice')}"}, timeout=10)
    assert r.status_code == 404
