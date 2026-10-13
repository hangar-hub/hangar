# Hub client compatibility suite

`compat/hf` runs real Hugging Face clients against Hangar's Hub API ([`03-hf-api.md`](../../docs/spec/03-hf-api.md)): `huggingface_hub` 2.2.0, the `hf` CLI, `transformers`, `sentence-transformers` and `datasets`. Fixtures are tiny models and datasets built in the tests, so the suite needs no network apart from the Hangar under test.

## Run it

```sh
uv run --project compat pytest compat/hf
```

With no other settings, `conftest.py` builds `compat/hf/server_test.go` into a test binary and starts it. That server is the Hub API from `internal/hfapi` over the fakes in `internal/hfapi/hfapitest`, including a stand-in for the git-lfs batch endpoint, which `internal/gitserver` serves in a full build.

| Variable | Effect |
|---|---|
| `HANGAR_HF_ENDPOINT`, `HANGAR_HF_TOKENS` | Test an Instance that is already running, such as `hangar server`. `HANGAR_HF_TOKENS` is JSON with Tokens for `alice` (admin scope, admin of the `acme` Organisation), `bob` (write scope, read Member of `acme`), `carol` (read scope) and `admin` (an Instance admin). `HANGAR_HF_ORG` renames `acme`. |
| `HANGAR_HF_GO_TAGS=w2real` | Build the test server with a `compat && w2real` backend file that uses the real `internal/repo`. See [`docs/build/handoffs/w4.md`](../../docs/build/handoffs/w4.md) for the file. |
| `HANGAR_HF_WRITE_MATRIX=1` | Write `compatibility-matrix.json` from the results. |

`go test -tags compat ./compat/hf` checks the test server starts and answers.

## What it covers

- `hf auth login --token`, `hf auth whoami`, `whoami-v2`, `hf download`, `hf upload`, `hf repos create`
- `create_repo` (public, private, dataset, Organisation, `exist_ok`), `delete_repo` (`missing_ok`), `move_repo` and the redirects after it, `update_repo_settings` (`private`, `visibility`, `gated`)
- `upload_file`, `upload_folder` (including LFS files and unchanged files), `create_commit` with adds, deletes, folder deletes and copies, `parent_commit`, `create_pr=True`
- `create_branch`, `create_tag`, `delete_branch`, `delete_tag`, `list_repo_refs`, `list_repo_commits`
- `model_info`, `dataset_info`, `list_models` and `list_datasets` with search, author, sort, limit, `full` and `expand`, `list_repo_files`, `list_repo_tree` (recursive, with `expand`), `get_paths_info`, `repo_exists`, `revision_exists`, `file_exists`
- `hf_hub_download` by branch, tag, Commit SHA, a branch with `/` in its name and `refs/pr/1`; `snapshot_download` with `allow_patterns`; offline reuse with `HF_HUB_OFFLINE=1`; resuming a partial download; downloads with `hf_xet` installed and enabled use plain HTTP
- `get_safetensors_metadata`, and `HfFileSystem` `ls`, `glob`, `info`, `exists`, `modified`, `open` (ranged reads), `cat_file`, writes and `rm`
- error mapping: `RepositoryNotFoundError` (401 without a token, 404 with one, for private Repos), `RevisionNotFoundError`, `EntryNotFoundError`, `GatedRepoError` (401 and 403), a bad token, write permissions
- discussions, comments, renames, status changes, pull requests and merges; Collections and their items; unlike
- `transformers` `AutoModel` and `AutoTokenizer`; a pickle-only model loads without any connection leaving the machine, so the safetensors conversion service is never contacted; `sentence-transformers`; `datasets.load_dataset`, also streaming
- the Xet write token endpoint, and `xet-read-token` answering 404

## Not covered here

- Xet uploads through `hf_xet` need the CAS endpoints: `compat/xet` (W11) covers them. Upload tests here run with Xet turned off, as with `HF_HUB_DISABLE_XET=1`.
- The git-lfs batch endpoint itself, including multipart: `compat/git` (W3).
- Pull-through: `compat/upstream` (W6).
- The client matrix in [13](../../docs/spec/13-testing.md) also asks for the oldest `huggingface_hub` that the latest `transformers` supports, and Python 3.10. The lock file pins the latest releases only; the matrix needs a CI job (`.github/` is W9's), listed in `docs/build/requests/w4.md`.
- `diffusers` (nightly in 13) is not installed.
