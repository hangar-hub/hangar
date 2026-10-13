"""Repo management through huggingface_hub: create, settings, move, delete,
existence checks, info and search."""

import pytest
import requests
from huggingface_hub import HfApi
from huggingface_hub.errors import HfHubHTTPError, RepositoryNotFoundError

pytestmark = pytest.mark.usefixtures("no_xet")


@pytest.mark.endpoint("POST /api/repos/create", "GET /api/models/{repo}", "GET /api/datasets/{repo}")
def test_create_repo_variants(api, hub, repo_name):
    url = api.create_repo(repo_name)
    assert url.repo_id == f"alice/{repo_name}"
    assert url.endpoint == hub.endpoint
    # exist_ok swallows the 409 and still returns the URL.
    again = api.create_repo(repo_name, exist_ok=True)
    assert again.repo_id == url.repo_id
    with pytest.raises(HfHubHTTPError) as err:
        api.create_repo(repo_name)
    assert err.value.response.status_code == 409

    private = api.create_repo(f"{repo_name}-p", private=True)
    assert api.model_info(private.repo_id).private is True
    ds = api.create_repo(f"{hub.org}/{repo_name}-ds", repo_type="dataset")
    assert ds.repo_type == "dataset"
    info = api.dataset_info(ds.repo_id)
    assert info.id == f"{hub.org}/{repo_name}-ds"
    assert info.sha
    # A fresh Repo has main with the Hub's default .gitattributes.
    assert [s.rfilename for s in api.model_info(url.repo_id).siblings] == [".gitattributes"]


@pytest.mark.endpoint("PUT /api/{type}s/{repo}/settings")
def test_update_repo_settings(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.update_repo_settings(repo, private=True)
    assert api.model_info(repo).private is True
    api.update_repo_settings(repo, visibility="public", gated="manual")
    info = api.model_info(repo)
    assert info.private is False
    assert info.gated == "manual"
    api.update_repo_settings(repo, gated=False)
    assert api.model_info(repo).gated is False


@pytest.mark.endpoint("POST /api/repos/move", "redirects for moved Repos")
def test_move_repo_redirects(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.upload_file(path_or_fileobj=b"hello", path_in_repo="a.txt", repo_id=repo)
    new = f"{hub.org}/{repo_name}-moved"
    api.move_repo(repo, new)
    assert api.model_info(new).id == new
    # The old ID keeps working for API calls and downloads.
    assert api.model_info(repo).id == new
    path = api.hf_hub_download(repo, "a.txt")
    assert open(path, "rb").read() == b"hello"


@pytest.mark.endpoint("DELETE /api/repos/delete")
def test_delete_repo(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.delete_repo(repo)
    assert not api.repo_exists(repo)
    with pytest.raises(RepositoryNotFoundError):
        api.delete_repo(repo)
    api.delete_repo(repo, missing_ok=True)


@pytest.mark.endpoint("GET /api/models/{repo}/revision/{rev}", "GET /api/{type}s/{repo}/revision/{rev}")
def test_exists_checks(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.upload_file(path_or_fileobj=b"{}", path_in_repo="config.json", repo_id=repo)
    assert api.repo_exists(repo)
    assert not api.repo_exists(f"alice/{repo_name}-nope")
    assert api.revision_exists(repo, "main")
    assert not api.revision_exists(repo, "nope")
    assert api.file_exists(repo, "config.json")
    assert not api.file_exists(repo, "nope.json")


@pytest.mark.endpoint("GET /api/models", "GET /api/datasets")
def test_list_models_and_datasets(api, hub, repo_name):
    api.create_repo(f"{repo_name}-alpha")
    api.create_repo(f"{repo_name}-beta", private=True)
    api.create_repo(f"{repo_name}-rows", repo_type="dataset")
    found = {m.id for m in api.list_models(search=repo_name)}
    assert found == {f"alice/{repo_name}-alpha", f"alice/{repo_name}-beta"}
    anon = HfApi(endpoint=hub.endpoint, token=False)
    assert {m.id for m in anon.list_models(search=repo_name)} == {f"alice/{repo_name}-alpha"}
    assert {m.id for m in api.list_models(author="alice", search=repo_name, limit=1)} <= found
    assert len(list(api.list_models(search=repo_name, limit=1))) == 1
    full = list(api.list_models(search=f"{repo_name}-alpha", full=True))
    assert [s.rfilename for s in full[0].siblings] == [".gitattributes"]
    assert [d.id for d in api.list_datasets(search=repo_name)] == [f"alice/{repo_name}-rows"]
    assert list(api.list_models(search=repo_name, sort="lastModified"))
    assert list(api.list_models(search=repo_name, sort="createdAt", expand=["sha", "private"]))


@pytest.mark.endpoint("GET /api/models/{repo}")
def test_model_info_shape(api, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.upload_folder(folder_path=_tree({"config.json": b'{"model_type":"bert"}', "unet/config.json": b"{}"}), repo_id=repo)
    info = api.model_info(repo, files_metadata=True)
    assert info.sha and len(info.sha) == 40
    assert info.last_modified is not None and info.created_at is not None
    assert {s.rfilename for s in info.siblings} == {".gitattributes", "config.json", "unet/config.json"}
    assert all(s.size is not None and s.blob_id for s in info.siblings)
    sec = api.model_info(repo, securityStatus=True)
    assert sec.security_repo_status is not None
    expanded = api.model_info(repo, expand=["downloads", "likes", "sha"])
    assert expanded.sha == info.sha


@pytest.mark.endpoint("POST /api/{type}s/{repo}/like", "DELETE /api/{type}s/{repo}/like")
def test_unlike(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    r = requests.post(f"{hub.endpoint}/api/models/{repo}/like", headers={"authorization": f"Bearer {hub.token('bob')}"}, timeout=10)
    assert r.status_code == 200
    assert api.model_info(repo).likes == 1
    HfApi(endpoint=hub.endpoint, token=hub.token("bob")).unlike(repo)
    assert api.model_info(repo).likes == 0


def _tree(files: dict[str, bytes]):
    import tempfile
    from pathlib import Path

    root = Path(tempfile.mkdtemp(prefix="hangar-hf-tree-"))
    for name, content in files.items():
        p = root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(content)
    return root
