"""Discussions, pull requests and Collections through huggingface_hub."""

import pytest
import requests
from huggingface_hub import CommitOperationAdd, HfApi
from huggingface_hub.errors import HfHubHTTPError

pytestmark = pytest.mark.usefixtures("no_xet")


@pytest.mark.endpoint(
    "GET /api/{type}s/{repo}/discussions",
    "POST /api/{type}s/{repo}/discussions",
    "GET /api/{type}s/{repo}/discussions/{num}",
    "POST /api/{type}s/{repo}/discussions/{num}/comment",
    "POST /api/{type}s/{repo}/discussions/{num}/status",
    "POST /api/{type}s/{repo}/discussions/{num}/title",
    "POST /api/{type}s/{repo}/discussions/{num}/merge",
)
def test_discussions_and_pull_requests(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    api.upload_file(path_or_fileobj=b"v1", path_in_repo="f.txt", repo_id=repo)
    bob = HfApi(endpoint=hub.endpoint, token=hub.token("bob"))

    d = bob.create_discussion(repo, "Question about f.txt", description="Is v1 final?")
    assert d.num == 1 and not d.is_pull_request
    comment = api.comment_discussion(repo, d.num, "Not yet.")
    assert comment.content == "Not yet."
    api.rename_discussion(repo, d.num, "Is f.txt final?")
    api.change_discussion_status(repo, d.num, "closed", comment="Answered")
    details = api.get_discussion_details(repo, d.num)
    assert details.title == "Is f.txt final?"
    assert details.status == "closed"
    assert [e.type for e in details.events][:2] == ["comment", "comment"]

    pr = bob.create_commit(repo, [CommitOperationAdd("f.txt", b"v2")], commit_message="Update f.txt", create_pr=True)
    assert pr.pr_num == 2
    prs = list(api.get_repo_discussions(repo, discussion_type="pull_request"))
    assert [p.num for p in prs] == [2]
    assert {x.num for x in api.get_repo_discussions(repo)} == {1, 2}
    api.merge_pull_request(repo, pr.pr_num, comment="Thanks")
    assert api.get_discussion_details(repo, pr.pr_num).status == "merged"
    with pytest.raises(HfHubHTTPError):
        bob.merge_pull_request(repo, pr.pr_num)


@pytest.mark.endpoint(
    "GET /api/collections",
    "POST /api/collections",
    "GET /api/collections/{slug}",
    "PATCH /api/collections/{slug}",
    "DELETE /api/collections/{slug}",
    "POST /api/collections/{slug}/items",
    "PATCH /api/collections/{slug}/items/{id}",
    "DELETE /api/collections/{slug}/items/{id}",
)
def test_collections(api, hub, repo_name):
    model = api.create_repo(repo_name).repo_id
    dataset = api.create_repo(f"{repo_name}-rows", repo_type="dataset").repo_id
    c = api.create_collection(f"Picks {repo_name}", description="Small things")
    assert c.slug.startswith("alice/")
    assert api.create_collection(f"Picks {repo_name}", exists_ok=True).slug == c.slug
    c = api.add_collection_item(c.slug, item_id=model, item_type="model", note="Tiny model")
    c = api.add_collection_item(c.slug, item_id=dataset, item_type="dataset")
    api.add_collection_item(c.slug, item_id=model, item_type="model", exists_ok=True)
    assert [i.item_id for i in c.items] == [model, dataset]
    assert c.items[0].note == "Tiny model"
    api.update_collection_item(c.slug, c.items[1].item_object_id, position=0, note="Rows")
    c = api.update_collection_metadata(c.slug, title=f"Best {repo_name}", private=True, theme="green")
    assert c.private is True
    got = api.get_collection(c.slug)
    assert [i.item_id for i in got.items][0] == dataset
    assert c.slug in {x.slug for x in api.list_collections(owner="alice")}
    # A private Collection is hidden from others.
    r = requests.get(f"{hub.endpoint}/api/collections/{c.slug}", headers={"authorization": f"Bearer {hub.token('bob')}"}, timeout=10)
    assert r.status_code == 404
    api.delete_collection_item(c.slug, got.items[0].item_object_id)
    assert len(api.get_collection(c.slug).items) == 1
    api.delete_collection(c.slug)
    api.delete_collection(c.slug, missing_ok=True)


@pytest.mark.endpoint(
    "GET /api/{type}s/{repo}/likers",
    "GET /api/users/{user}/likes",
    "POST /api/{type}s/{repo}/like",
)
def test_likers_and_liked_repos(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    bob = HfApi(endpoint=hub.endpoint, token=hub.token("bob"))
    # huggingface_hub has unlike() but no like(); the web UI likes Repos.
    r = requests.post(f"{hub.endpoint}/api/models/{repo}/like", headers={"authorization": f"Bearer {hub.token('bob')}"}, timeout=10)
    assert r.status_code == 200
    likers = list(api.list_repo_likers(repo))
    assert len(likers) == 1
    assert likers[0].fullname
    likes = api.list_liked_repos("bob")
    assert likes.user == "bob"
    assert repo in likes.models
    bob.unlike(repo)
    assert list(api.list_repo_likers(repo)) == []
    with pytest.raises(HfHubHTTPError):
        api.list_liked_repos("nobody-by-this-name")


@pytest.mark.endpoint(
    "POST /api/{type}s/{repo}/discussions/{num}/comment/{id}/edit",
    "POST /api/{type}s/{repo}/discussions/{num}/comment/{id}/hide",
)
def test_edit_and_hide_comments(api, hub, repo_name):
    repo = api.create_repo(repo_name).repo_id
    bob = HfApi(endpoint=hub.endpoint, token=hub.token("bob"))
    d = bob.create_discussion(repo, "Typo", description="There is a typo.")
    comment = bob.comment_discussion(repo, d.num, "It is in README.md, line 3.")
    edited = bob.edit_discussion_comment(repo, d.num, comment.id, "It is in README.md, line 4.")
    assert edited.content == "It is in README.md, line 4."
    assert edited.edited
    with pytest.raises(HfHubHTTPError):
        HfApi(endpoint=hub.endpoint, token=hub.token("carol")).edit_discussion_comment(repo, d.num, comment.id, "Spam")
    with pytest.warns(UserWarning):
        hidden = api.hide_discussion_comment(repo, d.num, comment.id)
    assert hidden.hidden
    details = api.get_discussion_details(repo, d.num)
    assert [e for e in details.events if e.id == comment.id][0].hidden
