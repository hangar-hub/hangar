# Xet upload compatibility suite

`compat/xet` checks UP-7: `huggingface_hub` with `hf_xet` installed uploads to Hangar with no client setup, and Hangar stores every file as a whole LFS object with the source file's SHA-256. The contract is in [`docs/spec/03-hf-api.md`](../../docs/spec/03-hf-api.md) ("Xet") and [`docs/research/xet-upload-research.md`](../../docs/research/xet-upload-research.md).

## How it runs

Each test runs real `huggingface_hub` and `hf_xet` clients in their own processes, each with its own `HF_HOME`, so the `hf_xet` cache (which is keyed by the CAS URL) belongs to that client alone.

The clients talk to a Go test server, `server_test.go`: the Hub API from `internal/hfapi` (through `hfapitest`), with the real `xet.Service` and CAS handler from `internal/xet` in front of it. The server has two test-only hooks: `/_test/stats` counts CAS calls, and `/_test/hold-shards` holds shard uploads so a test can kill a client mid-upload. The server needs `internal/hfapi` (W4), so it builds only with the `xetserver` tag and runs on a branch that has W4's code.

| Build tags | Backend |
|---|---|
| `compat,xetserver` | `hfapitest`'s real-git Repo fake, an in-memory Blob store for xorbs, SQLite for the Xet tables |
| `compat,xetserver,xetreal` | W2's `internal/repo` and W1's filesystem Blob store, sharing one database (SQLite, or Postgres with `HANGAR_XET_DB=postgres`) and one Blob store for git data, LFS objects and xorbs |

Set `HANGAR_XET_GO_TAGS=xetreal` to build the second.

## Running it

From `compat/`, with the `hf_xet` version under test:

```sh
uv run --no-project --python 3.11 --with pytest==9.1.1 --with requests==2.34.2 \
  --with huggingface_hub==2.2.0 --with hf-xet==1.7.0 pytest xet -p no:cacheprovider
```

Run it with `hf-xet==1.6.0` (the oldest version `huggingface_hub` 2.2.0 accepts) and with the latest 1.x. Add `HANGAR_XET_GO_TAGS=xetreal` for the real backend. The whole suite takes about four minutes; `test_token_expiry_during_a_long_upload` takes about two of them, and `test_v2_shard_heartbeats_behind_a_proxy_with_30s_idle_timeout` about one.

## What it covers

| Test | Checks |
|---|---|
| `test_upload_file` | `upload_file` of 7 KiB, 5 MiB random and 3 MiB text files through Xet: xorbs inserted, `/v2/shards` used, no 401 |
| `test_upload_with_each_compression_scheme` | `HF_XET_XORB_COMPRESSION_POLICY` set to `lz4`, `bg4-lz4` and `none` |
| `test_upload_large_file_across_xorbs` | a 70 MiB file needs at least two 64 MiB xorbs |
| `test_upload_folder` | `upload_folder` with 15 files in subfolders, several files per xorb and a duplicate file |
| `test_create_pr` | `create_pr=True`: the file is on `refs/pr/1` |
| `test_same_file_to_a_second_repo_is_deduplicated` | the second Repo's upload sends no xorb (deduplicated through the client cache); a modified copy sends one new xorb |
| `test_resume_after_killing_the_client` | the client is killed while its shard upload is held; the next upload with the same cache sends no xorb again and commits the file |
| `test_token_expiry_during_a_long_upload` | 40-second tokens and a slowly read xorb: the xorb POST runs past its token's expiry (tokens are checked when a request starts), the shard POST uses a refreshed token, and no CAS call gets a 401 |
| `test_v2_shard_heartbeats_behind_a_proxy_with_30s_idle_timeout` | a TCP proxy drops connections silent for 30 seconds; reassembly takes 45 seconds; heartbeats every 20 seconds keep `/v2/shards` alive |
| `test_v1_shard_fallback` | `HF_XET_CLIENT_SHARD_API_VERSION=1` uses `/v1/shards` |
| `test_reject_mode` | `hfapi.xet: reject`: small files upload as regular files, larger ones are refused at the token request, and `HF_HUB_DISABLE_XET=1` uploads them over LFS |
| `test_xet_read_token_is_not_offered` | `xet-read-token` is 404; the write token is a JSON body with matching `X-Xet-*` headers |

Every upload test downloads the file again and checks its SHA-256 against the source, and checks the LFS object ID in `paths-info`.

## Known client behaviour

- In `reject` mode, `hf_xet` fetches the write token itself and reports only `HTTP status client error (400 Bad Request)` with the token URL. The `X-Error-Message` that says to set `HF_HUB_DISABLE_XET=1` is not shown, so the docs must map that error to the fix.
