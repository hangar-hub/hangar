"""upload_large_folder: resumable, multi-commit uploads of large folders.

huggingface_hub 1.x has HfApi.upload_large_folder. huggingface_hub 2.x
removed it: its upload_folder commits large folders in batches through
hf_xet instead, which needs the Xet CAS endpoints (W11). The floor cell
(huggingface_hub 1.31.0) and the diffusers cell (1.33.0) run this module:

    UV_PROJECT_ENVIRONMENT=<dir> uv run --frozen --project compat --python 3.10 \\
        --with huggingface_hub==1.31.0 python -m pytest compat/hf

With huggingface_hub 2.x the module is reported as skipped, with this reason.
"""

import hashlib
import os
from pathlib import Path

import pytest
from huggingface_hub import HfApi

if not hasattr(HfApi, "upload_large_folder"):
    pytest.skip(
        "huggingface_hub 2.x has no upload_large_folder; the floor and diffusers matrix cells run it "
        "(see module docstring)",
        allow_module_level=True,
    )

pytestmark = pytest.mark.usefixtures("no_xet")


def _write(root: Path, files: dict[str, bytes]) -> Path:
    for name, content in files.items():
        p = root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(content)
    return root


@pytest.mark.endpoint(
    "upload_large_folder (huggingface_hub 1.x)",
    "POST /api/{type}s/{repo}/preupload/{rev}",
    "POST /api/{type}s/{repo}/commit/{rev}",
    "POST /{repo}.git/info/lfs/objects/batch (stand-in)",
)
@pytest.mark.parametrize("repo_type", ["model", "dataset"])
def test_upload_large_folder(api, repo_name, repo_type, tmp_path):
    repo = f"alice/{repo_name}"
    files = {f"data/shard-{i:02d}.txt": f"row {i}\n".encode() for i in range(40)}
    files["weights/model.safetensors"] = os.urandom(150_000)
    files["weights/other.bin"] = os.urandom(20_000)
    files["notes.log"] = b"ignored"
    folder = _write(tmp_path / "large", files)

    # upload_large_folder creates the Repo itself.
    api.upload_large_folder(
        repo_id=repo,
        folder_path=folder,
        repo_type=repo_type,
        ignore_patterns=["*.log"],
        num_workers=2,
        print_report=False,
    )
    files.pop("notes.log")
    remote = set(api.list_repo_files(repo, repo_type=repo_type))
    assert set(files) <= remote
    assert "notes.log" not in remote
    meta = {e.path: e for e in api.get_paths_info(repo, list(files), repo_type=repo_type)}
    assert meta["weights/model.safetensors"].lfs is not None
    assert meta["weights/model.safetensors"].lfs.sha256 == hashlib.sha256(files["weights/model.safetensors"]).hexdigest()
    for name in ("weights/model.safetensors", "weights/other.bin", "data/shard-07.txt"):
        assert Path(api.hf_hub_download(repo, name, repo_type=repo_type)).read_bytes() == files[name]

    # Running it again resumes from the local metadata: nothing to commit.
    head = api.repo_info(repo, repo_type=repo_type).sha
    api.upload_large_folder(repo_id=repo, folder_path=folder, repo_type=repo_type, ignore_patterns=["*.log"], print_report=False)
    assert api.repo_info(repo, repo_type=repo_type).sha == head

    # A changed file is uploaded and committed on the next run.
    (folder / "data/shard-07.txt").write_bytes(b"changed\n")
    api.upload_large_folder(repo_id=repo, folder_path=folder, repo_type=repo_type, ignore_patterns=["*.log"], print_report=False)
    assert api.repo_info(repo, repo_type=repo_type).sha != head
    path = api.hf_hub_download(repo, "data/shard-07.txt", repo_type=repo_type, force_download=True)
    assert Path(path).read_bytes() == b"changed\n"
