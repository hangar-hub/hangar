"""Error mapping: the exceptions huggingface_hub raises for Hangar's answers."""

import pytest
import requests
from huggingface_hub import HfApi, hf_hub_download
from huggingface_hub.errors import (
    EntryNotFoundError,
    GatedRepoError,
    HfHubHTTPError,
    RepositoryNotFoundError,
    RevisionNotFoundError,
)

pytestmark = pytest.mark.usefixtures("no_xet")


@pytest.fixture
def repos(api, repo_name):
    public = api.create_repo(repo_name).repo_id
    private = api.create_repo(f"{repo_name}-private", private=True).repo_id
    gated = api.create_repo(f"{repo_name}-gated").repo_id
    for r in (public, private, gated):
        api.upload_file(path_or_fileobj=b"{}", path_in_repo="config.json", repo_id=r)
    api.update_repo_settings(gated, gated="manual")
    return public, private, gated


@pytest.mark.endpoint("error mapping")
def test_not_found_errors(api, hub, repos, tmp_path):
    public, _, _ = repos
    with pytest.raises(RepositoryNotFoundError):
        api.model_info(f"{public}-missing")
    with pytest.raises(RevisionNotFoundError):
        api.model_info(public, revision="nope")
    with pytest.raises(RevisionNotFoundError):
        hf_hub_download(public, "config.json", revision="nope", cache_dir=tmp_path)
    with pytest.raises(EntryNotFoundError):
        hf_hub_download(public, "missing.json", cache_dir=tmp_path)
    with pytest.raises(EntryNotFoundError):
        list(api.list_repo_tree(public, path_in_repo="missing-dir"))


@pytest.mark.endpoint("error mapping")
def test_private_repos_are_hidden(hub, repos, tmp_path):
    _, private, _ = repos
    # Anonymous callers get 401 and others 404, both RepoNotFound.
    anon = HfApi(endpoint=hub.endpoint, token=False)
    with pytest.raises(RepositoryNotFoundError) as err:
        anon.model_info(private)
    assert err.value.response.status_code == 401
    bob = HfApi(endpoint=hub.endpoint, token=hub.token("bob"))
    with pytest.raises(RepositoryNotFoundError) as err:
        bob.model_info(private)
    assert err.value.response.status_code == 404
    with pytest.raises(RepositoryNotFoundError):
        hf_hub_download(private, "config.json", token=hub.token("bob"), cache_dir=tmp_path)


@pytest.mark.endpoint("error mapping")
def test_gated_repo(hub, repos, tmp_path):
    _, _, gated = repos
    bob = HfApi(endpoint=hub.endpoint, token=hub.token("bob"))
    assert bob.model_info(gated).gated == "manual"
    assert bob.repo_exists(gated)
    with pytest.raises(GatedRepoError) as err:
        hf_hub_download(gated, "config.json", token=hub.token("bob"), cache_dir=tmp_path)
    assert err.value.response.status_code == 403
    with pytest.raises(GatedRepoError) as err:
        hf_hub_download(gated, "config.json", token=False, cache_dir=tmp_path)
    assert err.value.response.status_code == 401


@pytest.mark.endpoint("bad token")
def test_bad_token(hub, repos):
    public, _, _ = repos
    with pytest.raises(HfHubHTTPError) as err:
        HfApi(endpoint=hub.endpoint, token="hf_wrong").model_info(public)
    assert err.value.response.status_code == 401
    assert not isinstance(err.value, RepositoryNotFoundError)
    r = requests.get(f"{hub.endpoint}/api/models/{public}", headers={"authorization": "Bearer hf_wrong"}, timeout=10)
    assert r.headers["X-Error-Message"] == "Invalid credentials in Authorization header"


@pytest.mark.endpoint("permissions")
def test_write_permissions(hub, repos):
    public, _, _ = repos
    for user in ("bob", "carol"):
        with pytest.raises(HfHubHTTPError) as err:
            HfApi(endpoint=hub.endpoint, token=hub.token(user)).upload_file(path_or_fileobj=b"x", path_in_repo="x.txt", repo_id=public)
        assert err.value.response.status_code == 403
