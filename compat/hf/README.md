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
| `HANGAR_HF_DB=postgres` | With `HANGAR_HF_GO_TAGS=w2real`, run the real `internal/repo` on Postgres instead of SQLite. `conftest.py` sets `HANGAR_TEST_POSTGRES=1` for the test server (a Postgres container, so Docker must be running) unless `HANGAR_TEST_POSTGRES_DSN` names a server. |
| `HANGAR_HF_WRITE_MATRIX=1` | Write `compatibility-matrix.json` from the results. |
| `HANGAR_HF_MULTIPART_THRESHOLD` | The tested Instance's `lfs.multipart_threshold`, with `HANGAR_HF_ENDPOINT` (default 64 MiB). The Go test server lowers it to 5 MiB, the smallest the setting allows. |
| `HANGAR_HF_MULTIPART_FILE_SIZE` | Size in bytes of the file `test_upload_over_multipart_threshold` uploads. The default is twice the threshold plus one byte; 13-testing.md asks for 200 MB on pull requests and a 6 GiB sparse file nightly. |
| `HANGAR_HF_XET_UPLOADS=1` | Leave Xet on, so the upload tests go through `hf_xet`. Needs an Instance that serves the Xet CAS endpoints (W11); the Go test server does not. |
| `HANGAR_HF_WITHOUT_XET=1` | Tell the suite `hf_xet` is not installed, for the matrix cell without it. |

`go test -tags compat ./compat/hf` checks the test server starts and answers.

## Client matrix

13-testing.md asks for the latest `huggingface_hub` and the oldest release the latest `transformers` supports, with and without `hf_xet`, on Python 3.10 and the latest Python. The floor is `huggingface_hub` 1.31.0: `transformers` 5.19.0 and `datasets` 5.1.0 need `huggingface-hub>=1.31.0,<3.0`. Each cell runs the whole suite; `UV_PROJECT_ENVIRONMENT` keeps the extra environments out of `compat/.venv`.

| Cell | Command |
|---|---|
| Latest client, Python 3.11, `hf_xet` installed (the lock) | `uv run --frozen --project compat python -m pytest compat/hf` |
| Latest Python | `UV_PROJECT_ENVIRONMENT=<dir> uv run --frozen --project compat --python 3.14 python -m pytest compat/hf` |
| Floor client, Python 3.10 | `UV_PROJECT_ENVIRONMENT=<dir> uv run --frozen --project compat --python 3.10 --with huggingface_hub==1.31.0 python -m pytest compat/hf` |
| Without `hf_xet` | `uv export --frozen --project compat --no-hashes --no-emit-project`, drop the `hf-xet` line, `uv pip install --no-deps` the rest into a new environment, then run its `python -m pytest compat/hf` with `HANGAR_HF_WITHOUT_XET=1` and its `bin` first on `PATH` |
| `diffusers` (nightly) | `uv run --frozen --project compat --with diffusers==0.41.0 --with huggingface_hub==1.33.0 python -m pytest compat/hf` |
| Xet uploads | `HANGAR_HF_XET_UPLOADS=1` against an Instance with W11's CAS endpoints |

`diffusers` 0.41 needs `huggingface_hub` below 2.0, so it is not in the lock; `test_diffusers.py` reports itself as skipped in the other cells. `huggingface_hub` 2.x removed `upload_large_folder`, so `test_upload_large_folder.py` runs in the floor and `diffusers` cells (`huggingface_hub` 1.31.0 and 1.33.0) and reports itself as skipped in the others. On the macOS host the suite was last run on (Darwin 27), the SciPy 1.15.3 wheel the lock picks for Python 3.10 fails to load with a `dyld` error about `__thread_bss`; add `--with scipy==1.14.1` to that cell there.

## What it covers

- `hf auth login --token`, `hf auth whoami`, `whoami-v2`, `hf download`, `hf upload`, `hf repos create`
- `create_repo` (public, private, dataset, Organisation, `exist_ok`), `delete_repo` (`missing_ok`), `move_repo` and the redirects after it, `update_repo_settings` (`private`, `visibility`, `gated`)
- `upload_file`, `upload_folder` (including LFS files and unchanged files), `create_commit` with adds, deletes, folder deletes and copies, `parent_commit`, `create_pr=True`; a `create_pr=True` commit that fails opens no pull request, also when the client retries
- `upload_large_folder` with `huggingface_hub` 1.x (models and datasets, ignore patterns, LFS files, a second run that resumes with nothing to commit, then a changed file)
- files over the multipart threshold: huggingface_hub's multipart path (numbered part `PUT`s with ETags, then the completion `POST`), checked byte for byte after download
- `create_branch`, `create_tag`, `delete_branch`, `delete_tag`, `list_repo_refs`, `list_repo_commits`
- `model_info`, `dataset_info`, `list_models` and `list_datasets` with search, author, sort, limit, `full` and `expand`, `list_repo_files`, `list_repo_tree` (recursive, with `expand`), `get_paths_info`, `repo_exists`, `revision_exists`, `file_exists`
- `hf_hub_download` by branch, tag, Commit SHA, a branch with `/` in its name and `refs/pr/1`; `snapshot_download` with `allow_patterns`; offline reuse with `HF_HUB_OFFLINE=1`; resuming a partial download; downloads with `hf_xet` installed and enabled use plain HTTP
- `get_safetensors_metadata`, and `HfFileSystem` `ls`, `glob`, `info`, `exists`, `modified`, `open` (ranged reads), `cat_file`, writes and `rm`
- error mapping: `RepositoryNotFoundError` (401 without a token, 404 with one, for private Repos), `RevisionNotFoundError`, `EntryNotFoundError`, `GatedRepoError` (401 and 403), a bad token, write permissions
- discussions, comments, renames, status changes, pull requests and merges; Collections and their items; unlike
- `transformers` `AutoModel` and `AutoTokenizer`; a pickle-only model loads without any connection leaving the machine, so the safetensors conversion service is never contacted; `sentence-transformers`; `datasets.load_dataset`, also streaming; a tiny `diffusers` pipeline (in its matrix cell)
- the Xet write token endpoint, and `xet-read-token` answering 404

## Not covered here

- Xet uploads through `hf_xet` need the CAS endpoints, which W11's `internal/xet` serves and `compat/xet` covers. Upload tests here run with Xet turned off, as with `HF_HUB_DISABLE_XET=1`, unless `HANGAR_HF_XET_UPLOADS=1`.
- The git-lfs batch endpoint itself is W3's (`compat/git`). Here the batch endpoint is `hfapitest`'s stand-in, with basic and multipart transfers over `repo.LFSStore`; rerun `test_upload_over_multipart_threshold` against W3's `internal/gitserver` once it is wired.
- Pull-through: `compat/upstream` (W6).
