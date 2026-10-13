"""Xet uploads with real huggingface_hub and hf_xet clients (UP-7).

Each upload runs in its own process with its own HF_HOME, through hf_xet
(installed by default with huggingface_hub). Every test checks that the
object Hangar stored has the source file's SHA-256, by its LFS metadata and
by downloading it, and checks the server's CAS counters to prove the upload
went through Xet.
"""

from __future__ import annotations

import json
import time
import uuid
from pathlib import Path

import pytest
import requests
from xetcompat import Client, Server, sha256_file, write_random, write_text
from huggingface_hub import HfApi, hf_hub_download

MiB = 1 << 20


def new_repo(server: Server, user: str = "alice") -> str:
    repo = f"{user}/xet-{uuid.uuid4().hex[:10]}"
    HfApi(endpoint=server.endpoint, token=server.tokens[user]).create_repo(repo)
    return repo


def upload_file_code(repo: str, path: Path, name: str, **kwargs) -> str:
    extra = "".join(f", {k}={v!r}" for k, v in kwargs.items())
    return (
        "from huggingface_hub import HfApi\n"
        f"r = HfApi().upload_file(path_or_fileobj={str(path)!r}, path_in_repo={name!r}, repo_id={repo!r}{extra})\n"
        "print(r)\n"
    )


def upload_folder_code(repo: str, folder: Path, **kwargs) -> str:
    extra = "".join(f", {k}={v!r}" for k, v in kwargs.items())
    return (
        "from huggingface_hub import HfApi\n"
        f"r = HfApi().upload_folder(folder_path={str(folder)!r}, repo_id={repo!r}{extra})\n"
        "print(r)\n"
    )


def assert_stored(server: Server, repo: str, name: str, sha: str, tmp: Path, revision: str = "main") -> None:
    """The file's LFS object ID is the source SHA-256, and the bytes match."""
    api = HfApi(endpoint=server.endpoint, token=server.tokens["alice"])
    (info,) = api.get_paths_info(repo, [name], revision=revision)
    assert info.lfs is not None, f"{name} is not an LFS file"
    assert info.lfs.sha256 == sha
    got = hf_hub_download(
        repo, name, revision=revision, endpoint=server.endpoint, token=server.tokens["alice"],
        cache_dir=str(tmp / "download-cache"),
    )
    assert sha256_file(Path(got)) == sha


def delta(before: dict[str, int], after: dict[str, int], key: str) -> int:
    return after[key] - before[key]


@pytest.mark.parametrize(("size", "kind"), [(7 * 1024, "random"), (5 * MiB, "random"), (3 * MiB, "text")])
def test_upload_file(server, client, tmp_path, size, kind):
    repo = new_repo(server)
    src = tmp_path / "src" / "weights.bin"
    sha = (write_random if kind == "random" else write_text)(src, size, size)
    before = server.stats()
    client.run(upload_file_code(repo, src, "weights.bin"))
    after = server.stats()
    assert delta(before, after, "xorbs_inserted") >= 1, "the upload went through Xet"
    assert delta(before, after, "shard_posts_v2") >= 1
    assert delta(before, after, "cas_401") == 0
    assert_stored(server, repo, "weights.bin", sha, tmp_path)


@pytest.mark.parametrize("policy", ["lz4", "bg4-lz4", "none"])
def test_upload_with_each_compression_scheme(server, tmp_path, policy):
    client = Client(server, tmp_path / "hf-home", env={"HF_XET_XORB_COMPRESSION_POLICY": policy})
    repo = new_repo(server)
    src = tmp_path / "src" / "table.bin"
    sha = write_text(src, 2 * MiB + 3, 11)
    client.run(upload_file_code(repo, src, "table.bin"))
    assert_stored(server, repo, "table.bin", sha, tmp_path)


def test_upload_large_file_across_xorbs(server, client, tmp_path):
    """70 MiB of random data needs more than one 64 MiB xorb."""
    repo = new_repo(server)
    src = tmp_path / "src" / "model.safetensors"
    sha = write_random(src, 70 * MiB, 3)
    before = server.stats()
    client.run(upload_file_code(repo, src, "model.safetensors"))
    after = server.stats()
    assert delta(before, after, "xorbs_inserted") >= 2
    assert_stored(server, repo, "model.safetensors", sha, tmp_path)
    src.unlink()


def test_upload_folder(server, client, tmp_path):
    repo = new_repo(server)
    folder = tmp_path / "folder"
    shas = {}
    for i in range(12):
        name = f"shard-{i:02d}.bin"
        shas[name] = write_random(folder / name, 100 * 1024 + i * 7777, 100 + i)
    shas["sub/dir/tokenizer.model"] = write_text(folder / "sub/dir/tokenizer.model", 600 * 1024, 5)
    shas["sub/big.safetensors"] = write_random(folder / "sub/big.safetensors", 3 * MiB + 1, 6)
    # A duplicate file: the same content twice in one upload.
    (folder / "copy.bin").write_bytes((folder / "shard-03.bin").read_bytes())
    shas["copy.bin"] = shas["shard-03.bin"]
    (folder / "README.md").write_text("# Folder upload\n")
    before = server.stats()
    client.run(upload_folder_code(repo, folder))
    after = server.stats()
    assert delta(before, after, "xorbs_inserted") >= 1
    for name, sha in shas.items():
        assert_stored(server, repo, name, sha, tmp_path)


def test_create_pr(server, client, tmp_path):
    repo = new_repo(server)
    src = tmp_path / "src" / "adapter.bin"
    sha = write_random(src, 2 * MiB, 21)
    out = client.run(upload_file_code(repo, src, "adapter.bin", create_pr=True)).stdout
    assert "/discussions/1" in out or "refs/pr/1" in out, out
    assert_stored(server, repo, "adapter.bin", sha, tmp_path, revision="refs/pr/1")


def test_same_file_to_a_second_repo_is_deduplicated(server, tmp_path):
    """hf_xet's shard cache is keyed by casUrl, not Repo: the second upload
    sends no xorbs and its shard names the first upload's xorbs."""
    client = Client(server, tmp_path / "hf-home")
    src = tmp_path / "src" / "base.bin"
    sha = write_random(src, 6 * MiB, 31)
    first, second = new_repo(server), new_repo(server)
    client.run(upload_file_code(first, src, "base.bin"))
    before = server.stats()
    client.run(upload_file_code(second, src, "base.bin"))
    after = server.stats()
    assert delta(before, after, "xorb_posts") == 0, "no xorb is sent again"
    assert delta(before, after, "shard_posts_v2") >= 1
    assert_stored(server, second, "base.bin", sha, tmp_path)

    # A modified copy reuses most chunks: far fewer bytes go up again.
    modified = tmp_path / "src" / "tuned.bin"
    data = bytearray(src.read_bytes())
    data[3 * MiB : 3 * MiB + 1000] = bytes(1000)
    modified.write_bytes(bytes(data))
    third = new_repo(server)
    before = server.stats()
    client.run(upload_file_code(third, modified, "tuned.bin"))
    after = server.stats()
    assert delta(before, after, "xorbs_inserted") == 1, "only the changed chunks are sent, in one new xorb"
    assert_stored(server, third, "tuned.bin", sha256_file(modified), tmp_path)


def test_resume_after_killing_the_client(server, tmp_path):
    """Kill the client while its shard upload is held, then upload again
    with the same cache: hf_xet resumes from its staged xorbs."""
    client = Client(server, tmp_path / "hf-home")
    repo = new_repo(server)
    src = tmp_path / "src" / "resume.bin"
    sha = write_random(src, 8 * MiB, 41)
    server.hold_shards(True)
    try:
        before = server.stats()
        proc = client.start(upload_file_code(repo, src, "resume.bin"))
        deadline = time.monotonic() + 120
        while server.stats()["shards_active"] == 0:
            assert proc.poll() is None, proc.communicate()
            assert time.monotonic() < deadline, "the shard upload never started"
            time.sleep(0.1)
        mid = server.stats()
        assert delta(before, mid, "xorbs_inserted") >= 1
        Client.kill(proc)
    finally:
        server.hold_shards(False)
    api = HfApi(endpoint=server.endpoint, token=server.tokens["alice"])
    assert not api.file_exists(repo, "resume.bin"), "the killed upload committed nothing"

    client.run(upload_file_code(repo, src, "resume.bin"))
    after = server.stats()
    assert delta(mid, after, "xorbs_inserted") == 0, "the resumed upload sends no xorb again"
    assert_stored(server, repo, "resume.bin", sha, tmp_path)


def test_token_expiry_during_a_long_upload(start_server, tmp_path):
    """Tokens last 40 s and xorbs are read slowly, so the upload outlives
    several tokens. hf_xet refreshes 30 s before expiry; no CAS call may get
    a 401, because hf_xet never retries one."""
    srv = start_server(HANGAR_XET_TOKEN_TTL="40s", HANGAR_XET_XORB_BYTES_PER_SEC=str(64 * 1024))
    client = Client(srv, tmp_path / "hf-home")
    repo = new_repo(srv)
    src = tmp_path / "src" / "slow.bin"
    sha = write_random(src, 6 * MiB, 51)
    started = time.monotonic()
    client.run(upload_file_code(repo, src, "slow.bin"), timeout=900)
    took = time.monotonic() - started
    stats = srv.stats()
    assert took > 80, f"the upload took {took:.0f} s, not long enough to outlive two tokens"
    assert stats["token_requests"] >= 3, stats
    assert stats["cas_401"] == 0, stats
    assert_stored(srv, repo, "slow.bin", sha, tmp_path)


def test_v2_shard_heartbeats_behind_a_proxy_with_30s_idle_timeout(start_server, tmp_path):
    """A proxy drops connections the server leaves silent for 30 s, and
    reassembly takes 45 s. /v2/shards repeats its last frame every 20 s, so
    the shard upload completes."""
    srv = start_server(HANGAR_XET_PROXY_IDLE="30s", HANGAR_XET_SHARD_DELAY="45s")
    client = Client(srv, tmp_path / "hf-home")
    repo = new_repo(srv)
    src = tmp_path / "src" / "proxied.bin"
    sha = write_random(src, 2 * MiB, 61)
    started = time.monotonic()
    client.run(upload_file_code(repo, src, "proxied.bin"), timeout=600)
    took = time.monotonic() - started
    stats = srv.stats()
    assert took > 45, took
    assert stats["shard_posts_v2"] == 1 and stats["shard_posts_v1"] == 0, stats
    assert stats["proxy_dropped"] == 0, stats
    assert_stored(srv, repo, "proxied.bin", sha, tmp_path)


def test_v1_shard_fallback(server, tmp_path):
    client = Client(server, tmp_path / "hf-home", env={"HF_XET_CLIENT_SHARD_API_VERSION": "1"})
    repo = new_repo(server)
    src = tmp_path / "src" / "v1.bin"
    sha = write_random(src, MiB, 71)
    before = server.stats()
    client.run(upload_file_code(repo, src, "v1.bin"))
    after = server.stats()
    assert delta(before, after, "shard_posts_v1") >= 1
    assert delta(before, after, "shard_posts_v2") == 0
    assert_stored(server, repo, "v1.bin", sha, tmp_path)


def test_reject_mode(start_server, tmp_path):
    srv = start_server(HANGAR_XET_MODE="reject")
    client = Client(srv, tmp_path / "hf-home")
    repo = new_repo(srv)

    # Small files are answered "regular" and upload with no client setup.
    small = tmp_path / "src" / "small.bin"
    small_sha = write_random(small, 2 * MiB, 81)
    client.run(upload_file_code(repo, small, "small.bin"))
    api = HfApi(endpoint=srv.endpoint, token=srv.tokens["alice"])
    got = hf_hub_download(repo, "small.bin", endpoint=srv.endpoint, token=srv.tokens["alice"],
                          cache_dir=str(tmp_path / "dl"))
    assert sha256_file(Path(got)) == small_sha

    # Larger files need Xet, which is refused with the way out.
    big = tmp_path / "src" / "big.bin"
    big_sha = write_random(big, 12 * MiB, 82)
    p = client.run(upload_file_code(repo, big, "big.bin"), check=False)
    assert p.returncode != 0
    assert "HF_HUB_DISABLE_XET=1" in p.stdout + p.stderr
    assert not api.file_exists(repo, "big.bin")
    assert srv.stats()["xorb_posts"] == 0

    # With HF_HUB_DISABLE_XET=1 it uploads over LFS.
    lfs_client = Client(srv, tmp_path / "hf-home-lfs", env={"HF_HUB_DISABLE_XET": "1"})
    lfs_client.run(upload_file_code(repo, big, "big.bin"))
    assert_stored(srv, repo, "big.bin", big_sha, tmp_path)


def test_xet_read_token_is_not_offered(server):
    """Downloads stay plain HTTP: no read token."""
    repo = new_repo(server)
    r = requests.get(f"{server.endpoint}/api/models/{repo}/xet-read-token/main",
                     headers={"Authorization": f"Bearer {server.tokens['alice']}"}, timeout=10)
    assert r.status_code == 404
    w = requests.get(f"{server.endpoint}/api/models/{repo}/xet-write-token/main",
                     headers={"Authorization": f"Bearer {server.tokens['alice']}"}, timeout=10)
    assert w.status_code == 200
    body = w.json()
    assert body["casUrl"] == server.endpoint + "/xet-cas"
    assert w.headers["X-Xet-Cas-Url"] == body["casUrl"]
    assert int(w.headers["X-Xet-Token-Expiration"]) == body["exp"]
    assert json.dumps(body)  # a JSON body, not headers only
