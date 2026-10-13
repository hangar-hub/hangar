"""Downloads through huggingface_hub: hf_hub_download by every kind of
Revision, snapshot_download, offline reuse, trees, paths-info, ranges,
safetensors metadata and HfFileSystem."""

import json
import os
import struct
import subprocess
import sys
from pathlib import Path

import pytest
from huggingface_hub import CommitOperationAdd, HfApi, HfFileSystem, hf_hub_download, snapshot_download

pytestmark = pytest.mark.usefixtures("no_xet")


def _safetensors(tensors: dict[str, tuple[str, list[int]]]) -> bytes:
    """A minimal safetensors file with zeroed tensors."""
    sizes = {"F32": 4, "I64": 8}
    header, offset, data = {}, 0, b""
    for name, (dtype, shape) in tensors.items():
        n = sizes[dtype]
        for d in shape:
            n *= d
        header[name] = {"dtype": dtype, "shape": shape, "data_offsets": [offset, offset + n]}
        offset += n
        data += b"\0" * n
    h = json.dumps(header).encode()
    return struct.pack("<Q", len(h)) + h + data


@pytest.fixture
def model_repo(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    st = _safetensors({"a": ("F32", [4, 4]), "b": ("I64", [3])})
    api.create_commit(
        repo,
        [
            CommitOperationAdd("config.json", b'{"model_type": "bert"}'),
            CommitOperationAdd("model.safetensors", st),
            CommitOperationAdd("onnx/model.onnx", b"onnx"),
            CommitOperationAdd("onnx/extra/weights.bin", os.urandom(1000)),
        ],
        commit_message="Add model",
    )
    return repo, st


@pytest.mark.endpoint("HEAD /{repo}/resolve/{rev}/{path}", "GET /{repo}/resolve/{rev}/{path}")
def test_hf_hub_download_by_revision(api, model_repo, tmp_path):
    repo, st = model_repo
    sha = api.model_info(repo).sha
    api.create_tag(repo, tag="v1")
    api.create_branch(repo, branch="dev/x")
    api.upload_file(path_or_fileobj=b'{"model_type": "gpt2"}', path_in_repo="config.json", repo_id=repo, revision="dev/x")
    HfApi(endpoint=api.endpoint, token=api.token).upload_file(
        path_or_fileobj=b'{"model_type": "pr"}', path_in_repo="config.json", repo_id=repo, create_pr=True
    )
    cache = tmp_path / "cache"
    for rev, want in [("main", b"bert"), ("v1", b"bert"), (sha, b"bert"), ("dev/x", b"gpt2"), ("refs/pr/1", b"pr")]:
        path = hf_hub_download(repo, "config.json", revision=rev, cache_dir=cache)
        assert want in Path(path).read_bytes(), rev
    path = hf_hub_download(repo, "model.safetensors", cache_dir=cache)
    assert Path(path).read_bytes() == st
    # local_dir downloads, and nested paths.
    local = hf_hub_download(repo, "onnx/model.onnx", local_dir=tmp_path / "local")
    assert Path(local).read_bytes() == b"onnx"


@pytest.mark.endpoint("GET /api/{type}s/{repo}/revision/{rev}", "GET /api/{type}s/{repo}/tree/{rev}/{path}")
def test_snapshot_download_and_offline(api, model_repo, tmp_path):
    repo, _ = model_repo
    cache = tmp_path / "cache"
    snap = Path(snapshot_download(repo, cache_dir=cache, allow_patterns=["*.json", "onnx/*"]))
    assert (snap / "config.json").exists()
    assert (snap / "onnx" / "model.onnx").exists()
    assert not (snap / "model.safetensors").exists()
    full = Path(snapshot_download(repo, cache_dir=cache))
    assert (full / "onnx" / "extra" / "weights.bin").exists()
    # The cache serves the same Revision with the network off.
    env = dict(os.environ, HF_HUB_OFFLINE="1")
    code = f"from huggingface_hub import snapshot_download; print(snapshot_download({repo!r}, cache_dir={str(cache)!r}))"
    out = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True, check=True)
    assert out.stdout.strip() == str(full)


@pytest.mark.endpoint("GET /api/{type}s/{repo}/tree/{rev}/{path}", "POST /api/{type}s/{repo}/paths-info/{rev}")
def test_tree_and_paths_info(api, model_repo):
    repo, st = model_repo
    tree = {e.path: e for e in api.list_repo_tree(repo, recursive=True)}
    assert {"onnx", "onnx/extra"} <= set(tree)
    assert tree["model.safetensors"].size == len(st)
    assert tree["model.safetensors"].lfs is not None
    top = [e.path for e in api.list_repo_tree(repo, path_in_repo="onnx")]
    assert sorted(top) == ["onnx/extra", "onnx/model.onnx"]
    expanded = list(api.list_repo_tree(repo, path_in_repo="onnx", expand=True))
    assert all(e.last_commit is not None for e in expanded if hasattr(e, "lfs"))
    info = api.get_paths_info(repo, ["config.json", "onnx", "missing.txt"])
    assert sorted(i.path for i in info) == ["config.json", "onnx"]
    files = api.list_repo_files(repo)
    assert "onnx/extra/weights.bin" in files


@pytest.mark.endpoint("GET /{repo}/resolve/{rev}/{path} (Range)")
def test_ranges_and_safetensors_metadata(api, model_repo):
    repo, st = model_repo
    meta = api.get_safetensors_metadata(repo)
    assert meta.files_metadata["model.safetensors"].parameter_count == {"F32": 16, "I64": 3}
    fs = HfFileSystem(endpoint=api.endpoint, token=api.token)
    with fs.open(f"{repo}/model.safetensors", "rb", block_size=16) as f:
        f.seek(8)
        assert f.read(4) == st[8:12]
    assert fs.cat_file(f"{repo}/model.safetensors", start=3, end=11) == st[3:11]


@pytest.mark.endpoint("HfFileSystem")
def test_hf_file_system(api, model_repo):
    repo, _ = model_repo
    fs = HfFileSystem(endpoint=api.endpoint, token=api.token)
    names = fs.ls(repo, detail=False)
    assert f"{repo}/config.json" in names
    assert f"{repo}/onnx" in names
    assert sorted(fs.glob(f"{repo}/**/*.bin")) == [f"{repo}/onnx/extra/weights.bin"]
    info = fs.info(f"{repo}/config.json")
    assert info["type"] == "file" and info["size"] == len(b'{"model_type": "bert"}')
    assert fs.info(f"{repo}/onnx")["type"] == "directory"
    assert fs.exists(f"{repo}@main/config.json")
    assert not fs.exists(f"{repo}/nope.json")
    assert fs.modified(f"{repo}/config.json") is not None
    with fs.open(f"{repo}/new.txt", "wb") as f:
        f.write(b"written through hf://")
    assert fs.cat_file(f"{repo}/new.txt") == b"written through hf://"
    fs.rm(f"{repo}/new.txt")
    assert not fs.exists(f"{repo}/new.txt")


@pytest.mark.endpoint("GET /{repo}/resolve/{rev}/{path} (no Xet)")
def test_download_with_hf_xet_installed(api, model_repo, tmp_path):
    """Downloads use plain HTTP whether or not hf_xet is installed and enabled."""
    repo, st = model_repo
    env = {k: v for k, v in os.environ.items() if k != "HF_HUB_DISABLE_XET"}
    code = (
        "import huggingface_hub, sys\n"
        "from huggingface_hub.utils._runtime import is_xet_available\n"
        "assert is_xet_available(), 'hf_xet should be installed and enabled'\n"
        f"p = huggingface_hub.hf_hub_download({repo!r}, 'model.safetensors', cache_dir={str(tmp_path)!r})\n"
        "sys.stdout.write(open(p, 'rb').read().hex())\n"
    )
    out = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True)
    assert out.returncode == 0, out.stderr
    assert bytes.fromhex(out.stdout) == st


@pytest.mark.endpoint("GET /{repo}/resolve/{rev}/{path} (resume)")
def test_resume_partial_download(api, model_repo, tmp_path):
    """http_get resumes with `Range: bytes=n-`; a 206 continues the file."""
    from huggingface_hub.file_download import hf_hub_url, http_get

    repo, st = model_repo
    url = hf_hub_url(repo, "model.safetensors", endpoint=api.endpoint)
    target = tmp_path / "partial"
    with open(target, "wb") as f:
        f.write(st[:50])
        http_get(url, f, resume_size=50, expected_size=len(st))
    assert target.read_bytes() == st
