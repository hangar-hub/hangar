"""Uploads through huggingface_hub: upload_file, upload_folder,
create_commit, upload_large_folder, LFS files (single part and multipart),
branches, tags and history."""

import hashlib
import math
import os
from pathlib import Path

import pytest
from huggingface_hub import CommitOperationAdd, CommitOperationCopy, CommitOperationDelete, HfApi
from huggingface_hub.errors import EntryNotFoundError, HfHubHTTPError

pytestmark = pytest.mark.usefixtures("no_xet")


def _write(root: Path, files: dict[str, bytes]) -> Path:
    for name, content in files.items():
        p = root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(content)
    return root


@pytest.mark.endpoint(
    "POST /api/validate-yaml",
    "POST /api/{type}s/{repo}/preupload/{rev}",
    "POST /api/{type}s/{repo}/commit/{rev}",
    "POST /{repo}.git/info/lfs/objects/batch (stand-in)",
)
def test_upload_file_and_folder(api, repo_name, tmp_path):
    repo = api.create_repo(repo_name).repo_id
    info = api.upload_file(path_or_fileobj=b"---\nlicense: mit\n---\n# Card\n", path_in_repo="README.md", repo_id=repo)
    assert info.oid and len(info.oid) == 40
    assert info.commit_url.startswith(api.endpoint + f"/{repo}/commit/")

    weights = os.urandom(200_000)
    folder = _write(tmp_path / "up", {"config.json": b'{"a": 1}', "model.safetensors": weights, "sub/notes.txt": b"n"})
    commit = api.upload_folder(folder_path=folder, repo_id=repo, commit_message="Add model")
    assert commit.oid
    files = set(api.list_repo_files(repo))
    assert files == {".gitattributes", "README.md", "config.json", "model.safetensors", "sub/notes.txt"}
    meta = {s.rfilename: s for s in api.model_info(repo, files_metadata=True).siblings}
    assert meta["model.safetensors"].lfs is not None
    assert meta["model.safetensors"].lfs.sha256 == hashlib.sha256(weights).hexdigest()
    assert meta["model.safetensors"].size == len(weights)
    path = api.hf_hub_download(repo, "model.safetensors")
    assert Path(path).read_bytes() == weights

    # Unchanged files are skipped: an upload of the same folder makes no Commit.
    again = api.upload_folder(folder_path=folder, repo_id=repo, commit_message="Again")
    assert again.oid == commit.oid


def _multipart_file_size(hub) -> int:
    """The size of the file test_upload_over_multipart_threshold uploads.

    13-testing.md asks for 200 MB on pull requests and a 6 GiB sparse file
    nightly; HANGAR_HF_MULTIPART_FILE_SIZE sets it. The default, two parts
    and one byte, keeps local runs small and still ends on a short part.
    """
    return int(os.environ.get("HANGAR_HF_MULTIPART_FILE_SIZE", 2 * hub.multipart_threshold + 1))


@pytest.mark.endpoint(
    "POST /api/{type}s/{repo}/preupload/{rev}",
    "POST /api/{type}s/{repo}/commit/{rev}",
    "POST /{repo}.git/info/lfs/objects/batch (stand-in, multipart)",
)
def test_upload_over_multipart_threshold(api, hub, repo_name, tmp_path, monkeypatch):
    from huggingface_hub import lfs

    size = _multipart_file_size(hub)
    assert size > hub.multipart_threshold, "HANGAR_HF_MULTIPART_FILE_SIZE must be over lfs.multipart_threshold"
    path = tmp_path / "big.bin"
    digest = hashlib.sha256()
    with path.open("wb") as f:
        # Random blocks, so parts sent out of order cannot hash the same.
        left = size
        while left:
            block = os.urandom(min(left, 1 << 20))
            f.write(block)
            digest.update(block)
            left -= len(block)

    # Record the multipart uploads huggingface_hub makes, so the test fails if
    # the server answers with a single PUT instead.
    multipart = []
    upload_multi_part = lfs._upload_multi_part

    def spy(*, operation, header, chunk_size, upload_url):
        multipart.append((operation.path_in_repo, chunk_size, sum(k.isdigit() for k in header)))
        return upload_multi_part(operation=operation, header=header, chunk_size=chunk_size, upload_url=upload_url)

    monkeypatch.setattr(lfs, "_upload_multi_part", spy)

    repo = api.create_repo(repo_name).repo_id
    api.upload_file(path_or_fileobj=str(path), path_in_repo="big.bin", repo_id=repo)
    assert len(multipart) == 1
    name, chunk_size, parts = multipart[0]
    assert name == "big.bin"
    assert parts == math.ceil(size / chunk_size) > 1

    meta = {s.rfilename: s for s in api.model_info(repo, files_metadata=True).siblings}
    assert meta["big.bin"].lfs is not None
    assert meta["big.bin"].lfs.sha256 == digest.hexdigest()
    assert meta["big.bin"].size == size

    # The same bytes under another name are already stored: nothing is sent.
    api.upload_file(path_or_fileobj=str(path), path_in_repo="copy/big.bin", repo_id=repo)
    assert len(multipart) == 1

    downloaded = hashlib.sha256()
    with open(api.hf_hub_download(repo, "copy/big.bin"), "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            downloaded.update(block)
    assert downloaded.hexdigest() == digest.hexdigest()


@pytest.mark.endpoint("POST /api/{type}s/{repo}/commit/{rev}", "POST /api/{type}s/{repo}/paths-info/{rev}")
def test_create_commit_adds_deletes_copies(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    big = os.urandom(50_000)
    api.create_commit(
        repo,
        [
            CommitOperationAdd("a.txt", b"a"),
            CommitOperationAdd("logs/1.txt", b"1"),
            CommitOperationAdd("logs/2.txt", b"2"),
            CommitOperationAdd("w.bin", big),
        ],
        commit_message="Add",
    )
    head = api.model_info(repo).sha
    api.create_commit(
        repo,
        [
            CommitOperationCopy("a.txt", "copy-of-a.txt"),
            CommitOperationCopy("w.bin", "copy-of-w.bin"),
            CommitOperationDelete("logs/"),
            CommitOperationDelete("a.txt"),
        ],
        commit_message="Copy and delete",
        parent_commit=head,
    )
    files = set(api.list_repo_files(repo))
    assert files == {".gitattributes", "copy-of-a.txt", "w.bin", "copy-of-w.bin"}
    assert Path(api.hf_hub_download(repo, "copy-of-w.bin")).read_bytes() == big
    # A stale parent is refused; deleting a missing file is EntryNotFound.
    with pytest.raises(HfHubHTTPError) as err:
        api.create_commit(repo, [CommitOperationAdd("b.txt", b"b")], commit_message="stale", parent_commit=head)
    assert err.value.response.status_code in (409, 412)
    with pytest.raises(EntryNotFoundError):
        api.create_commit(repo, [CommitOperationDelete("ghost.txt")], commit_message="delete ghost")


@pytest.mark.endpoint("POST /api/{type}s/{repo}/commit/{rev}", "POST /api/{type}s/{repo}/preupload/{rev}")
def test_upload_many_files(api, repo_name, tmp_path):
    # huggingface_hub 2.x folded upload_large_folder into upload_folder.
    repo = api.create_repo(repo_name).repo_id
    files = {f"shard-{i}.txt": f"row {i}\n".encode() for i in range(30)}
    files["weights.safetensors"] = os.urandom(30_000)
    folder = _write(tmp_path / "big", files)
    api.upload_folder(repo_id=repo, folder_path=folder, allow_patterns=["*.txt", "*.safetensors"], ignore_patterns=["shard-29.txt"])
    files.pop("shard-29.txt")
    assert set(api.list_repo_files(repo)) >= set(files)


@pytest.mark.endpoint(
    "POST /api/{type}s/{repo}/branch/{branch}",
    "DELETE /api/{type}s/{repo}/branch/{branch}",
    "POST /api/{type}s/{repo}/tag/{rev}",
    "DELETE /api/{type}s/{repo}/tag/{tag}",
    "GET /api/{type}s/{repo}/refs",
)
def test_branches_and_tags(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.create_branch(repo, branch="feature/x")
    api.create_branch(repo, branch="feature/x", exist_ok=True)
    api.upload_file(path_or_fileobj=b"x", path_in_repo="x.txt", repo_id=repo, revision="feature/x")
    api.create_tag(repo, tag="v1.0", tag_message="First", revision="feature/x")
    api.create_tag(repo, tag="v1.0", exist_ok=True)
    refs = api.list_repo_refs(repo)
    assert {b.name for b in refs.branches} == {"main", "feature/x"}
    assert [t.name for t in refs.tags] == ["v1.0"]
    assert refs.converts == []
    assert Path(api.hf_hub_download(repo, "x.txt", revision="v1.0")).read_bytes() == b"x"
    api.delete_tag(repo, tag="v1.0")
    api.delete_branch(repo, branch="feature/x")
    refs = api.list_repo_refs(repo)
    assert [b.name for b in refs.branches] == ["main"]
    assert refs.tags == []


@pytest.mark.endpoint("GET /api/{type}s/{repo}/commits/{rev}")
def test_list_repo_commits(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    for i in range(3):
        api.upload_file(path_or_fileobj=str(i).encode(), path_in_repo="n.txt", repo_id=repo, commit_message=f"Commit {i}")
    commits = api.list_repo_commits(repo)
    assert [c.title for c in commits[:3]] == ["Commit 2", "Commit 1", "Commit 0"]
    assert commits[0].commit_id == api.model_info(repo).sha
    assert commits[0].authors == ["alice"]
    formatted = api.list_repo_commits(repo, formatted=True)
    assert formatted[0].formatted_title


@pytest.mark.endpoint("POST /api/{type}s/{repo}/commit/{rev}?create_pr=1", "GET /api/{type}s/{repo}/discussions/{num}")
def test_hf_upload_to_new_branch_and_pr(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.upload_file(path_or_fileobj=b"main", path_in_repo="f.txt", repo_id=repo)
    bob = HfApi(endpoint=hub.endpoint, token=hub.token("bob"))
    pr = bob.upload_file(path_or_fileobj=b"proposal", path_in_repo="f.txt", repo_id=repo, create_pr=True, commit_message="Propose")
    assert pr.pr_url.endswith("/discussions/1")
    assert pr.pr_revision == "refs/pr/1"
    assert Path(api.hf_hub_download(repo, "f.txt", revision="refs/pr/1")).read_bytes() == b"proposal"
    assert Path(api.hf_hub_download(repo, "f.txt", revision="main", force_download=True)).read_bytes() == b"main"
    refs = api.list_repo_refs(repo, include_pull_requests=True)
    assert [p.ref for p in refs.pull_requests] == ["refs/pr/1"]
    details = api.get_discussion_details(repo, 1)
    assert details.is_pull_request
    assert details.target_branch == "refs/heads/main"
