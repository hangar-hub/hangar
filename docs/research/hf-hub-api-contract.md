# Hugging Face Hub wire contract for a self-hosted, HF-compatible registry

Implementer's reference for a Go server that real Hugging Face clients can use unmodified with `HF_ENDPOINT=https://registry.example`.

## 0. Sources and conventions

| Source | Version read |
|---|---|
| `huggingface/huggingface_hub` | **v2.2.0** (latest release, published 2026-10-08), tag commit `acfdf30d99441317270cca6e9362dd7f9ff47c55` |
| `huggingface/hub-docs` | `main` at `ef42b8f94576e58f9c32d5eed2f250a180dcc477` (2026-10-13), mainly `docs/xet/*` |
| `pydantic/httpx2` (HTTP client used by huggingface_hub 2.x) | `main` at `d9893857220fa8b294f4caa28a0fd684c256f097` (2026-10-12): redirect, encoding and Link parsing only |
| Live probes of `https://huggingface.co` | 2026-10-13, anonymous `curl` against public repos (`openai-community/gpt2`, `hf-internal-testing/tiny-random-bert`, `HuggingFaceFW/fineweb`) |
| Downstream libraries | See section 11 (transformers, diffusers, sentence-transformers, datasets, vLLM) |

All file references are relative to `src/huggingface_hub/` in huggingface_hub v2.2.0 unless stated otherwise.

**Tags**
- **MUST**: a client crashes or behaves incorrectly without it.
- **SHOULD**: clients work without it but degrade (lost features, extra round trips, poorer errors).
- **MAY**: optional, nice to have, or only used by niche commands.

**Big-picture facts that shape everything below**

1. huggingface_hub 2.x uses **`httpx2`** (`setup.py`: `httpx2>=2.0.0,<3`; pydantic's httpx fork, read at `pydantic/httpx2@d9893857`), not `requests`. The shared client is built with `follow_redirects=True` (`utils/_http.py::default_client_factory`). Query and form encoding follow httpx conventions: booleans become `true`/`false` (`httpx2/_utils.py::primitive_value_to_str`), and list values become **repeated keys** (`expand=a&expand=b`; `_content.py::encode_urlencoded_data`). Older 0.x clients (still in many images) used `requests`, which encodes booleans as `True`/`False`. Parse booleans case-insensitively and accept `1`/`0`.
2. `hf_xet` is a **default dependency** on x86_64, amd64, arm64 and aarch64 (`setup.py`, `install_requires`). When it is importable and `HF_HUB_DISABLE_XET` is not set, **uploads always go through Xet, with no server negotiation**. See section 8. This is the riskiest compatibility point.
3. `{repo_id}` is put into URLs **unencoded** (`ns/name`). Revisions are encoded with `quote(rev, safe="")`, so `refs/pr/1` becomes `refs%2Fpr%2F1`. Go's `net/http` decodes `%2F` in `r.URL.Path`, so route on `r.URL.EscapedPath()` (or `RawPath`) and unescape each segment after splitting.
4. Every JSON date the client parses goes through `utils/_datetime.py::parse_datetime`, which **only accepts `YYYY-MM-DDTHH:MM:SS[.fraction]Z`**. Emit UTC with a literal `Z`, for example `2024-11-16T03:16:16.000Z`. `+00:00` raises `ValueError`.

### Path prefixes per repo type

| Purpose | Model | Dataset |
|---|---|---|
| File resolve | `/{ns}/{name}/resolve/{rev}/{path}` | `/datasets/{ns}/{name}/resolve/{rev}/{path}` |
| REST API | `/api/models/{ns}/{name}/...` | `/api/datasets/{ns}/{name}/...` |
| LFS batch | `/{ns}/{name}.git/info/lfs/objects/batch` | `/datasets/{ns}/{name}.git/info/lfs/objects/batch` |
| Repo web URL (returned in JSON) | `{base}/{ns}/{name}` | `{base}/datasets/{ns}/{name}` |

Sources: `constants.py` (`REPO_TYPES_URL_PREFIXES`, `HUGGINGFACE_CO_URL_TEMPLATE`), `file_download.py::hf_hub_url`, `lfs.py::post_lfs_batch_info`, and `hf_api.py` methods that use `f"{self.endpoint}/api/{repo_type}s/{repo_id}/..."`. Spaces (`spaces/`) and kernels (`kernels/`) follow the same pattern and are out of scope.

**Single-segment repo ids** (`gpt2`): `utils/_validators.py::validate_repo_id` still allows them, but `HfFileSystem.resolve_path` rejects them ("Single-segment ids ... are no longer supported"), and `RepoUrl` parsing requires `ns/name` (`utils/_hf_uris.py::_url_to_uri_body`). **MUST**: namespace every repo. **MAY**: answer single-segment ids with a 307 to the canonical id, as the Hub does (see section 10).

### Request headers clients send

| Header | Value | Source |
|---|---|---|
| `authorization` | `Bearer <token>` when a token is configured (`HF_TOKEN`, token file, or explicit). Omitted when there is no token or `HF_HUB_DISABLE_IMPLICIT_TOKEN=1` | `utils/_headers.py::build_hf_headers`, `get_token_to_send` |
| `user-agent` | `unknown/None; hf_hub/2.2.0; python/3.x; torch/x; ...` or `<library>/<ver>; hf_hub/...` | `utils/_headers.py::_http_user_agent` |
| `X-Amzn-Trace-Id` | random UUID per request (request id) | `utils/_http.py::hf_request_event_hook` |
| `X-HF-Download-Counter` | `1` on every call made inside `hf_hub_download` / `snapshot_download` | `utils/_http.py::flag_as_download_call` |
| `Accept-Encoding` | `identity` on the metadata HEAD only. Other requests use the httpx default | `file_download.py::get_hf_file_metadata` |

The client includes `X-Request-Id`, `X-Amzn-Trace-Id` or `X-Amz-Cf-Id` from responses in error messages (`utils/_http.py::_format`). **SHOULD**: echo an `X-Request-Id` response header.

---

## 1. Download path

### 1.1 Call sequence

**`hf_hub_download(repo_id, filename, revision=R)`** (`file_download.py::hf_hub_download` → `_hf_hub_download_to_cache_dir` / `_hf_hub_download_to_local_dir`):

1. If `R` is a full 40-hex commit hash and `snapshots/R/filename` exists in the cache, return it. **No network.**
2. `HEAD {base}/[datasets/]{repo}/resolve/{quote(R, safe="")}/{quote(filename)}` (`get_hf_file_metadata`), with `Accept-Encoding: identity` and redirects followed manually (section 1.3). Timeout is `etag_timeout`, 10 s by default (`HF_HUB_ETAG_TIMEOUT`). There are **no retries** on the first attempt (`retry_on_errors=False`). A second attempt with retries and a 60 s timeout happens only if the first failed with a network error, 408/429/5xx, and nothing is cached.
3. If the blob `blobs/{etag}` already exists, symlink it and stop.
4. `GET {url_to_download}` (`http_get`), streamed in 10 MiB chunks, with read timeout `HF_HUB_DOWNLOAD_TIMEOUT` (10 s). `url_to_download` is the `Location` from the HEAD when it differs from the request URL. The `authorization` header is **dropped** when that location is not on a Hub origin (`_is_same_or_hub_host`).

**`snapshot_download(repo_id, revision=R)`** (`_snapshot_download.py::snapshot_download`):

1. If `R` is not a 40-hex sha: `GET /api/{type}s/{repo}/revision/{quote(R)}` (`HfApi.repo_info` → `model_info`/`dataset_info`). The client asserts `sha` is not `None`.
2. Unless the tree is cached on disk (`trees/{sha}.json`, `_tree_cache.py`): `GET /api/{type}s/{repo}/tree/{sha}?recursive=true&expand=false`, following `Link: rel="next"` pages (`HfApi.list_repo_tree`). **Note that the revision is the resolved sha**, so tree, resolve and HEAD must accept a full commit hash as `{rev}`.
3. For each file (8 threads): `hf_hub_download(..., revision=sha)`, which does a HEAD and then a GET per file.

**`HfApi.resolve_revision`** (new in 2.x) issues the same `repo_info` call and caches `refs/{rev}`.

### 1.2 `HEAD|GET /{repo}/resolve/{rev}/{path}`: **MUST**

**Request**
- `{rev}`: branch, tag, full sha, or special ref, percent-encoded with `safe=""` (`feature%2Fx`, `refs%2Fpr%2F3`, `refs%2Fconvert%2Fparquet`). **SHOULD** also accept unencoded special refs (`refs/pr/N`, `refs/convert/<word>`), matching the client's own regex `hf_api.py::SPECIAL_REFS_REVISION_REGEX`.
- `{path}`: `quote(filename)` with `/` kept, so non-ASCII is UTF-8 percent-encoded.
- Optional `Range` (section 1.5).
- Go note: Go 1.22 `ServeMux` `GET` patterns also match `HEAD`. Make sure HEAD returns the same headers, **including an explicit `Content-Length`**, and no body.

**Response headers the client reads** (`file_download.py::get_hf_file_metadata`, `utils/_xet.py::parse_xet_file_data_from_response`)

| Header | Read as | Rule for our server |
|---|---|---|
| `X-Repo-Commit` | `commit_hash`; a missing value raises `FileMetadataError` ("does not seem to be served by a Hugging Face Hub endpoint") | **MUST** on every 200 and on any 3xx that points off-origin. Full 40-hex sha of the resolved revision. Also **SHOULD** be on `EntryNotFound` 404s (section 1.6). |
| `X-Linked-Etag`, then `ETag` | `etag = _normalize_etag(X-Linked-Etag or ETag)`, which strips `W/` and quotes. A missing value raises `FileMetadataError` | **MUST**. LFS file: `"<sha256 hex of content>"`. Regular (git) file: `"<git blob sha1>"`, where sha1 = `sha1("blob {len}\0" + bytes)` (`utils/sha.py::git_hash`). Send both `ETag` and `X-Linked-Etag` with the same quoted value. |
| `X-Linked-Size`, then `Content-Length` | `size`. `Content-Length` is **ignored on redirects** (`None if response.is_redirect`) | **MUST** give the real byte size: `Content-Length` on a 200, `X-Linked-Size` on any 3xx. |
| `Location` | `location`; otherwise the final request URL | Only with a 3xx. |
| `X-Xet-Hash`, `Link: <...>; rel="xet-auth"`, `X-Xet-Refresh-Route` | Xet metadata. If present and `hf_xet` is installed, the download goes through Xet | **MUST NOT** send these (section 8). |
| `Content-Range`, `Content-Length`, `Content-Encoding` (GET) | expected size: `Content-Range` total wins, then `Content-Length`. Any non-`identity` `Content-Encoding` makes the size unknown | **MUST NOT** content-encode resolve responses. `Content-Range` total must be numeric; `bytes 0-9/*` crashes `int()` in `_get_file_length_from_http_response`. |
| `Content-Disposition` | display name only (`HEADER_FILENAME_PATTERN`) | **MAY**: `inline; filename*=UTF-8''x; filename="x";` |
| `X-HF-Warning: topic; message` | logged once per topic (`utils/_http.py::_warn_on_warning_headers`) | **MAY** |

How the client uses the ETag: it becomes the blob filename in the cache (`blobs/{etag}`), the dedup key, and, for `local_dir` downloads whose metadata is missing, a value matched against `^[0-9a-f]{64}$`. A sha256-looking ETag triggers a local sha256 recompute to skip re-downloads (`_hf_hub_download_to_local_dir`). **Content MUST match the ETag format** above. That keeps caches interchangeable with huggingface.co and lets `hf cache verify` (`HfApi.verify_repo_checksums`, `utils/_verification.py`) check LFS files by sha256 and regular files by git sha1.

**Live Hub reference (2026-10-13)**

Regular file:
```
HEAD /openai-community/gpt2/resolve/main/config.json
HTTP/2 307
location: /api/resolve-cache/models/openai-community/gpt2/607a30d7.../config.json?...&etag=%2210c6...%22
x-repo-commit: 607a30d783dfa663caf39e06633721c8d4cfcd7e
x-linked-etag: "10c66461e4c109db5a2196bff4bb59be30396ed8"
  (same-origin redirect, followed by the client)
HTTP/2 200
content-length: 665
etag: "10c66461e4c109db5a2196bff4bb59be30396ed8"
x-repo-commit: 607a30d783dfa663caf39e06633721c8d4cfcd7e
accept-ranges: bytes
```
LFS/Xet file:
```
HEAD /openai-community/gpt2/resolve/main/model.safetensors
HTTP/2 302
location: https://us.aws.cdn.hf.co/xet-bridge-us/...   (other origin, so the client stops here)
x-repo-commit: 607a30d7...
x-linked-size: 548105171
x-linked-etag: "248dfc3911869ec493c76e65bf2fcf7f615828b0254c12b473182f0f81d3a707"
x-xet-hash: 63bed808...                                 <- we must NOT send this
link: <https://huggingface.co/api/models/openai-community/gpt2/xet-read-token/607a...>; rel="xet-auth", ...
```

**What our server should return (recommended)**

A. Serve the bytes directly (simplest):
```
HTTP/1.1 200 OK
Content-Length: 548105171
ETag: "248dfc39...a707"
X-Linked-Etag: "248dfc39...a707"
X-Linked-Size: 548105171
X-Repo-Commit: 607a30d783dfa663caf39e06633721c8d4cfcd7e
Accept-Ranges: bytes
Content-Type: application/octet-stream
Content-Disposition: inline; filename*=UTF-8''model.safetensors; filename="model.safetensors";
```

B. Redirect LFS blobs to object storage (MinIO/S3 presigned URL) on a **different origin**:
```
HTTP/1.1 302 Found
Location: https://blobs.example/bucket/sha256/248d...?X-Amz-Signature=...
X-Repo-Commit: 607a...
X-Linked-Etag: "248d..."
X-Linked-Size: 548105171
```
The GET then goes to the blob host **without** `authorization`, so the URL must be presigned. The GET also needs `Range` support (section 1.5).

**Trap**: do not redirect to a content-addressed path on the **same origin** (for example `302 /blobs/<sha256>`). The client follows same-origin redirects on HEAD and reads `X-Repo-Commit`, `ETag` and size from the **final** response (`utils/_http.py::_httpx2_follow_hub_redirects_with_backoff`, `file_download.py::get_hf_file_metadata`), so the blob endpoint would also have to emit `X-Repo-Commit`. The Hub works around this by carrying the commit in the redirect target path (`/api/resolve-cache/.../{sha}/...`). A same-origin target is fine only if it also emits the full header set.

**Performance**: HEAD has a 10 s timeout with no retry, and then falls back to cache or fails. sha256, git sha1 and size **MUST** be stored at commit time, never computed per request. GET read timeout is 10 s between bytes, so streaming from slow backends must start within 10 s and must not stall.

**Size limit in the client**: `http_get` raises `ValueError("The file is too large to be downloaded using the regular download method ... Install hf_xet")` when `expected_size > 50 GB` on a fresh download (`file_download.py::http_get`, `constants.MAX_HTTP_DOWNLOAD_SIZE = 50*1000*1000*1000`). Files over 50 GB **cannot** be fetched by `hf_hub_download` over plain HTTP when the size is known. Workaround (**MAY**, hacky): for files over 50 GB, answer the HEAD with an off-origin 302 **without** `X-Linked-Size`. Size then becomes `None`, the check is skipped, and the size is learned from the GET. `HfFileSystem.get_file` takes `expected_size` from the tree listing and would still refuse. Plan to split weights into shards under 50 GB (normal for safetensors).

### 1.3 Redirect handling

| Request | Mechanism | Behaviour |
|---|---|---|
| HEAD metadata (`get_hf_file_metadata`, `file_exists`) | `utils/_http.py::_httpx2_follow_hub_redirects_with_backoff`: `follow_redirects=False`, manual loop up to 20 hops | Follows a 3xx with `Location` (relative is resolved with `urljoin`) **only if** the target origin (scheme, host, port) equals the current URL's origin or a known Hub origin (`huggingface.co`, `hub-ci.huggingface.co`, `hf.co`, the `HF_ENDPOINT` origin; `_HUB_ORIGINS`). Otherwise it returns the 3xx response itself as the metadata source. `hf_raise_for_status` never raises on 3xx. |
| GET download (`http_get`), API calls, HfFileSystem range reads | shared `httpx2.Client(follow_redirects=True)` | Redirects are followed automatically to any host. httpx2 strips `Authorization` when the origin changes, except on a same-host http→https upgrade (`httpx2/_client.py::BaseClient._redirect_headers`, pydantic/httpx2 `d9893857`). `hf_hub_download` additionally pops `authorization` itself before the GET when the HEAD location is off-Hub (`_get_metadata_or_catch_error`). |

Both relative and absolute `Location` values work. Use **307/308** for anything that may carry a body (commit, preupload, LFS batch, paths-info, LFS PUT). In httpx2, **302 turns every non-HEAD method (including POST, PUT and DELETE) into GET**, 303 turns non-HEAD into GET, and 301 turns POST into GET (`httpx2/_client.py::BaseClient._redirect_method`). 307 and 308 keep the method and body (`_redirect_stream`).

### 1.4 Revision handling, offline mode, and commit pinning

- Revision encoding: section 0, point 3. **MUST** decode `%2F` inside the `{rev}` segment. Branch and tag names containing `/` are legal.
- `{rev}` may be a branch, tag, full sha, or `refs/pr/N`. Short shas: the client accepts `[A-Fa-f0-9]{5,40}` only for `parent_commit` (`constants.REGEX_COMMIT_OID`). **MAY** resolve short shas elsewhere.
- **Commit pinning**: when the revision is a full 40-hex sha, `hf_hub_download` skips all network if the file is cached. `snapshot_download` skips `repo_info`, and skips the tree call when `trees/{sha}.json` is cached (`_snapshot_download.py`, `_tree_cache.py`). The client stores `refs/{branch}` → sha from `X-Repo-Commit` / `sha`. **MUST** treat commits as immutable: the same sha always returns the same bytes and the same tree.
- `HF_HUB_OFFLINE=1` (or `TRANSFORMERS_OFFLINE=1`) blocks every request in the event hook (`OfflineModeIsEnabled`). Nothing is server-side.
- Fallback to cache: on a network error, 5xx, or any `HfHubHTTPError` other than `RevisionNotFound`/`EntryNotFound`, the client serves the cached file for `refs/{rev}` if present (`_hf_hub_download_to_cache_dir`). 401 or `RepositoryNotFound`/`GatedRepo` with nothing cached is re-raised (`_raise_on_head_call_error`).

### 1.5 Range and resume: **MUST** for HfFileSystem and safetensors metadata, **SHOULD** for `hf_hub_download`

| Caller | Range sent | Requirement |
|---|---|---|
| `http_get` in-process retry after `ConnectError`/`TimeoutException`/`RemoteProtocolError` (5 retries; `_adjust_range_header`) | `bytes={n}-` | On a 200 instead of a 206, the client truncates and restarts (`resume_size > 0 and status == 200`). Works, but wastes bandwidth. In v2.x there is **no cross-process resume**: each download uses a unique `*.{uuid}.incomplete` tmp file (`_download_to_tmp_and_move`). |
| `HfFileSystemFile._fetch_range` (fsspec block reads, used by `datasets`, pandas and pyarrow over `hf://`) | `bytes={start}-{end-1}` | **MUST return 206 with exactly that slice.** The client returns `r.content` without checking the status, so a 200 with the full body corrupts reads. |
| `HfFileSystemStreamFile._open_connection` (reconnect) | `bytes={loc}-` | 206. A **416 at EOF** is treated as "already fully read". |
| `HfApi.parse_safetensors_file_metadata` (`get_safetensors_metadata`, used by vLLM and others) | `bytes=0-100000`, then `bytes=8-{N+7}` | **MUST** honour Range. A full-body 200 on the second call breaks the header parse. |

Use `Content-Range: bytes a-b/TOTAL` with a numeric `TOTAL`, and `Accept-Ranges: bytes`. Single ranges only: the client rejects multi-range in `_adjust_range_header`. Go's `http.ServeContent` or `ServeContent` over an `io.ReadSeeker` from object storage covers this.

### 1.6 Download errors (status + `X-Error-Code`)

See section 9 for the full mapping. For resolve specifically:

| Situation | Status | Headers | Client effect |
|---|---|---|---|
| File missing at a resolved revision | 404 | `X-Error-Code: EntryNotFound`, `X-Error-Message: Entry not found`, **`X-Repo-Commit: <sha>`** | `RemoteEntryNotFoundError`. With `X-Repo-Commit`, the client writes `.no_exist/{sha}/{file}` and `refs/{rev}`, so later probes (transformers optional files) are answered from cache (`_get_metadata_or_catch_error`). |
| Revision missing | 404 | `X-Error-Code: RevisionNotFound` | `RevisionNotFoundError`, raised immediately with no cache fallback |
| Repo missing or private without access | 404 (or 401) | `X-Error-Code: RepoNotFound` | `RepositoryNotFoundError` |
| Gated without access | 403 (or 401) | `X-Error-Code: GatedRepo` | `GatedRepoError` (subclass of `RepositoryNotFoundError`) |

---

## 2. Repo info

### 2.1 `GET /api/models/{repo}` and `GET /api/models/{repo}/revision/{rev}`: **MUST**
### 2.2 `GET /api/datasets/{repo}` and `GET /api/datasets/{repo}/revision/{rev}`: **MUST**

Client: `hf_api.py::HfApi.model_info`, `dataset_info`, `repo_info`; used by `snapshot_download`, `resolve_revision`, `repo_exists`, `revision_exists`, `HfFileSystem._repo_and_revision_exist`, `hf upload` (branch check), and `create_commit` (empty-commit path).

**Request**
- The path uses `/revision/{quote(rev, safe="")}` only when a revision is passed. `snapshot_download` always passes one (default `main`). HfFileSystem passes `None` unless the path has `@rev`.
- Query (model): `securityStatus=true` (adds `securityRepoStatus`), `blobs=true` (from `files_metadata=True`: adds `size`, `blobId`, `lfs` to siblings), `expand=<prop>` **repeated key per property** (httpx list encoding, **not** `expand[]`). Dataset: `blobs`, `expand`. `expand` cannot be combined with `blobs`/`securityStatus` (client-side check). **SHOULD** accept `expand`, `expand[]`, and ignore unknown values.
- Model expand values (`ExpandModelProperty_T`): `author, baseModels, cardData, childrenModelCount, config, createdAt, disabled, downloads, downloadsAllTime, evalResults, gated, gguf, inference, inferenceProviderMapping, lastModified, library_name, likes, mask_token, model-index, pipeline_tag, private, resourceGroup, safetensors, sha, siblings, spaces, tags, transformersInfo, trendingScore, usedStorage, widgetData`.
- Dataset expand values (`ExpandDatasetProperty_T`): `author, cardData, citation, createdAt, description, disabled, downloads, downloadsAllTime, gated, lastModified, likes, mainSize, paperswithcode_id, private, resourceGroup, sha, siblings, tags, trendingScore, usedStorage`.
- Returning more fields than requested is safe: unknown keys go into `__dict__` (`ModelInfo.__init__`, last line).

**Response 200** (live shape of `/api/models/hf-internal-testing/tiny-random-bert/revision/main?blobs=true`, trimmed):
```json
{
  "_id": "621ffdc136468d709f17ba4c",
  "id": "hf-internal-testing/tiny-random-bert",
  "modelId": "hf-internal-testing/tiny-random-bert",
  "author": "hf-internal-testing",
  "sha": "f171d7baecaf37b5da5a3616d8833b9969753535",
  "lastModified": "2024-11-16T03:16:16.000Z",
  "createdAt": "2022-03-02T23:29:05.000Z",
  "private": false,
  "gated": false,
  "disabled": false,
  "downloads": 0,
  "likes": 0,
  "library_name": "transformers",
  "pipeline_tag": null,
  "tags": ["transformers", "safetensors", "bert"],
  "config": {"model_type": "bert"},
  "transformersInfo": {"auto_model": "AutoModel"},
  "safetensors": {"parameters": {"F32": 126339, "I64": 512}, "total": 126851},
  "cardData": null,
  "spaces": [],
  "usedStorage": 76418830,
  "siblings": [
    {"rfilename": ".gitattributes", "blobId": "cefbc1394f3d04a79ceecc9eb31c9fec4511b168", "size": 737},
    {"rfilename": "config.json", "blobId": "22ffb3454131a71d4144340befb799c66ad0c670", "size": 548},
    {"rfilename": "model.safetensors", "blobId": "<git sha1 of LFS pointer>", "size": 510000,
     "lfs": {"sha256": "<sha256 of content>", "size": 510000, "pointerSize": 134}}
  ]
}
```
Without `blobs=true`, siblings carry only `{"rfilename": ...}`.

**Field requirements**, from `hf_api.py::ModelInfo.__init__`, `DatasetInfo.__init__` and `RepoSibling`:

| Field | Requirement | Notes |
|---|---|---|
| `id` | **MUST** (`kwargs.pop("id")`, KeyError otherwise) | `ns/name` |
| `sha` | **MUST** (`snapshot_download` and `resolve_revision` `assert sha is not None`) | full 40-hex commit of the requested revision |
| `siblings[].rfilename` | **MUST** if `siblings` is present (KeyError) | transformers, diffusers and vLLM read siblings (section 11). Always return `siblings`. |
| `siblings[].lfs` | if present and truthy, **MUST** have `size`, `sha256`, `pointerSize` (KeyError) | **Note the key is `sha256` here but `oid` in tree entries.** |
| `siblings[].size`, `blobId` | optional (`.get`) | **SHOULD** when `blobs=true` |
| `lastModified`, `createdAt` | optional; if present **MUST** be `...Z` format (`parse_datetime`) | |
| `private`, `disabled` | optional bool | **SHOULD** |
| `gated` | optional: `false`, `"auto"` or `"manual"` | **SHOULD** send `false` |
| `transformersInfo` | optional; if present **MUST** contain `auto_model` and **only** keys from `{auto_model, custom_class, pipeline_tag, processor}` (`TransformersInfo(**dict)` raises `TypeError` on unknown keys) | **MAY** omit |
| `safetensors` | optional; if truthy **MUST** have `parameters` (dict dtype→count) and `total` | **MAY** |
| `cardData` | optional dict, parsed leniently (`ModelCardData(..., ignore_metadata_errors=True)`) | **SHOULD**: YAML front matter of README.md as JSON |
| `inferenceProviderMapping` | optional; dict or list, otherwise `ValueError`; entries need `providerId`, `status`, `task` | **MUST NOT** send unless implemented |
| `evalResults` | optional; parsed by `parse_eval_result_entries` | **MUST NOT** send unless implemented |
| `author`, `tags`, `pipeline_tag`, `library_name`, `downloads`, `likes`, `config`, `gguf`, `spaces`, `usedStorage`, `mask_token`, `widgetData`, `model-index`, `baseModels`, ... | optional, passed through | **SHOULD**: `author`, `tags`, `library_name`, `pipeline_tag`, `config` (some loaders read it; section 11) |
| Dataset extras: `description`, `citation`, `paperswithcode_id`, `mainSize` | optional | |

**Status codes**: 200; 404 + `RevisionNotFound` (unknown rev); 404 + `RepoNotFound`; 403/401 + `GatedRepo`. `revision_exists` treats `RevisionNotFound` and `RepoNotFound` as False. `repo_exists` treats `GatedRepo` as True and `RepoNotFound` as False. `hf upload --revision X` relies on `RevisionNotFound` (not `RepoNotFound`) to decide to create the branch (`cli/upload.py`).

---

## 3. Tree and paths-info

### 3.1 `GET /api/{type}s/{repo}/tree/{rev}[/{path}]`: **MUST**

Client: `HfApi.list_repo_tree` (also behind `list_repo_files`, `snapshot_download`, `verify_repo_checksums`, `HfFileSystem.ls/find/glob/walk`).

**Request**
- `{rev}`: `quote(rev, safe="")`, default `main`. `snapshot_download` passes the resolved sha.
- `{path}`: `"/" + quote(path_in_repo, safe="")`, so **slashes inside the path arrive as `%2F` in a single segment** (`tree/main/a%2Fb`). **MUST** decode. **SHOULD** also accept raw slashes.
- Query: **always** `recursive=true|false&expand=true|false` (`params={"recursive": recursive, "expand": expand}`).
- Pagination: the client follows `Link: <URL>; rel="next"` verbatim (`utils/_pagination.py::paginate`, `response.links["next"]["url"]`). Following pages are fetched with the same headers and **no extra params**: the next URL must carry everything.

**Response 200**: JSON array (live shapes):
```json
[
  {"type": "directory", "oid": "d03ec5ec179df58241d27d55f92a674f5f44197f", "size": 0, "path": "onnx"},
  {"type": "file", "oid": "10c66461e4c109db5a2196bff4bb59be30396ed8", "size": 665, "path": "config.json"},
  {"type": "file", "oid": "90e59f00f62c654b1a88a5f127dff14df4611cdc", "size": 125162496,
   "lfs": {"oid": "c966da3b74697803352ca7c6f2f220e7090a557b619de9da0c6b34d89f7825c1", "size": 125162496, "pointerSize": 134},
   "path": "64-8bits.tflite"}
]
```
With `expand=true`, entries also carry:
```json
"lastCommit": {"id": "8faa6d76...", "title": "Update 64-8bits.tflite", "date": "2019-12-12T15:44:08.000Z"},
"securityFileStatus": {"status": "safe", "avScan": {...}, "pickleImportScan": {...}}
```

**Entry requirements** (`hf_api.py::RepoFile.__init__`, `RepoFolder.__init__`)

| Field | Requirement |
|---|---|
| `type` | **MUST**. `"file"` produces `RepoFile`; **any other value** (use `"directory"`) produces `RepoFolder` |
| `path` | **MUST**. Repo-relative, no leading slash |
| `oid` | **MUST** for both. File: git blob sha1 (for an LFS file, the sha1 **of the pointer file**). Directory: git tree sha1. Any stable string works for the client, but `hf cache verify` compares the file `oid` with the local git sha1 of non-LFS files. |
| `size` | **MUST** for files (`kwargs.pop("size")`). **The real content size, also for LFS** (HfFileSystem uses it as `expected_size`; the tree cache stores it) |
| `lfs` | for LFS files **MUST** have `oid` (sha256), `size`, `pointerSize` |
| `xetHash` | **MUST NOT** send (section 8) |
| `lastCommit` | optional; if present **MUST** have `id`, `title`, `date` (Z format) |
| `securityFileStatus` | optional; if present **MUST** have `status`, `avScan`, `pickleImportScan` (KeyError). **SHOULD** omit. |

**Recursive listing**: return every descendant (files **and** directories) with full paths. HfFileSystem builds its `dircache` from parent paths and expects directories to appear (`_ls_tree`). The Hub lists directories first; ordering is otherwise not relied on.

**Pagination format** (live): `link: <https://huggingface.co/api/datasets/HuggingFaceFW/fineweb/tree/main?expand=false&recursive=true&limit=1000&cursor=ZXlK...%3D>; rel="next"`. The cursor is opaque and server-defined. **MUST**: the URL must be **absolute** and use **our public base URL**, respecting `X-Forwarded-Proto/Host` behind a proxy. The client does not rewrite it to `HF_ENDPOINT`. `Response.links` returns the raw string, and the shared client has no `base_url`, so a relative URL cannot be requested (`httpx2/_models.py::Response.links`, `_client.py::_merge_url`). Omit `Link` on the last page. Page size of 1000 matches the Hub (**MAY**).

**Errors** (live): a non-existent path **or a path that is a file** returns `404` + `X-Error-Code: EntryNotFound` + `{"error":"config.json does not exist on \"main\""}`. HfFileSystem relies on this: `ls()` catches `EntryNotFoundError` and retries on the parent (`hf_file_system.py::ls`). Unknown revision: 404 `RevisionNotFound`. Unknown repo: 404 `RepoNotFound`.

### 3.2 `POST /api/{type}s/{repo}/paths-info/{rev}`: **MUST** (HfFileSystem `info()`, `CommitOperationCopy`)

Client: `HfApi.get_paths_info`; batches of up to 500 paths from `_commit_api.py::_fetch_files_to_copy` (`FETCH_LFS_BATCH_SIZE`).

**Request**: `application/x-www-form-urlencoded` (httpx `data=`), with a repeated `paths` key and a boolean string:
```
paths=config.json&paths=onnx&paths=nope.txt&expand=false
```
**SHOULD** also accept a JSON body `{"paths": [...], "expand": bool}`, which other clients use.

**Response 200**: array of tree entries (same shape as 3.1) **for existing paths only**. Missing paths are **silently omitted**, not 404 (live: `nope.txt` was simply absent). HfFileSystem maps an empty array to `FileNotFoundError` (`HfFileSystem.info`).

---

## 4. Refs, commits, branches, tags

### 4.1 `GET /api/{type}s/{repo}/refs[?include_prs=1]`: **SHOULD**

Client: `HfApi.list_repo_refs`; also the `create_branch(exist_ok=True)` fallback after a 403.
```json
{
  "branches": [{"name": "main", "ref": "refs/heads/main", "targetCommit": "f171d7ba..."}],
  "converts": [],
  "tags": [{"name": "v1.0", "ref": "refs/tags/v1.0", "targetCommit": "..."}],
  "pullRequests": [{"name": "1", "ref": "refs/pr/1", "targetCommit": "..."}]
}
```
`branches`, `converts` and `tags` are **MUST** keys (direct indexing). `pullRequests` is **MUST** when `include_prs=1` is sent. Each item **MUST** have `name`, `ref`, `targetCommit`. `refs/convert/*` refs go in `converts` (datasets uses `refs/convert/parquet`; section 11).

### 4.2 `GET /api/{type}s/{repo}/commits/{rev}[?expand[]=formatted]`: **SHOULD** (MUST for HfFileSystem `modified()` and root `info(expand_info=True)`)

Client: `HfApi.list_repo_commits`, paginated with `Link: rel="next"` (absolute URL). Note the **literal `expand[]`** key here.
```json
[
  {"id": "f171d7ba...", "title": "Upload ONNX weights (#2)", "message": "\n...",
   "authors": [{"user": "Xenova", "avatar": "https://..."}],
   "date": "2024-11-16T03:16:16.000Z",
   "formatted": {"title": "<p>...</p>", "message": "<p>...</p>"}}
]
```
**MUST** per item: `id`, `title`, `message`, `date` (Z format), and `authors[].user`. `formatted` is optional. Order is newest first. The Hub also sends `X-Total-Count` (**MAY**).

### 4.3 `POST /api/{type}s/{repo}/branch/{quote(branch, safe="")}`: **SHOULD** (`hf upload --revision`)
Body: `{}` or `{"startingPoint": "<rev unquoted>"}` (`HfApi.create_branch`). Response 200, body ignored. **409** if it exists: the client tolerates 409 when `exist_ok=True`, and `hf upload` always passes `exist_ok=True`. A 403 makes the client fall back to `list_repo_refs`.

### 4.4 `DELETE /api/{type}s/{repo}/branch/{quote(branch)}`: **MAY**. 200, body ignored.

### 4.5 `POST /api/{type}s/{repo}/tag/{quote(rev, safe="")}`: **MAY**
Body: `{"tag": "v1.0", "message": "optional"}` (`HfApi.create_tag`). The path segment is the **revision to tag** (default `main`), not the tag name. 409 if the tag exists (tolerated with `exist_ok`).

### 4.6 `DELETE /api/{type}s/{repo}/tag/{quote(tag, safe="")}`: **MAY**. 200.

---

## 5. Search and listing

### 5.1 `GET /api/models`: **SHOULD** (`HfApi.list_models`, `hf models ls`)
### 5.2 `GET /api/datasets`: **SHOULD** (`HfApi.list_datasets`, `hf datasets ls`)

**Query params sent** (`hf_api.py::list_models`, `list_datasets`)

| Param | Encoding | Notes |
|---|---|---|
| `search` | string (datasets: may repeat; `dataset_name` and `search` both go in the list) | substring match on id |
| `author` | string | namespace filter |
| `filter` | repeated key | tags such as `library:transformers` or `dataset:x`; datasets also prefix `language:`, `task_categories:`, `benchmark:` and so on |
| `sort` | `lastModified`, `trendingScore`, `createdAt`, `downloads`, `likes` | **no `direction` param in v2.x**. Older clients send `direction=-1`; **SHOULD** accept and treat as descending |
| `limit` | int | the client also stops iterating after `limit` items (`islice`) |
| `full` | `true` | return full objects (siblings and so on) |
| `config` | `true` | models: include `config` |
| `cardData` | `true` | include `cardData` |
| `expand` | repeated key | same values as 2.1; cannot be combined with `full`/`cardData`/`config` |
| `gated`, `pipeline_tag`, `apps`, `inference`, `inference_provider`, `num_parameters` | misc | **MAY** ignore |

**Response 200**: JSON array of objects shaped like section 2 (`id` **MUST**; if `siblings` is absent the client sets it to `None`). Pagination: absolute `Link: <...&cursor=...>; rel="next"` (live example: `link: <https://huggingface.co/api/models?limit=2&search=gpt2&cursor=eyIk...>; rel="next"`). With `limit` set, the Hub returns one page; the client handles both.

---

## 6. Auth

### 6.1 Token transport: **MUST**

`authorization: Bearer <token>` on every request when a token is configured (`utils/_headers.py::build_hf_headers`). Token precedence (`utils/_auth.py::get_token`): OIDC exchange (`HF_OIDC_RESOURCE`), then `HF_TOKEN` / `HUGGING_FACE_HUB_TOKEN`, then `~/.cache/huggingface/token` (with transparent OAuth refresh), then Colab. Tokens starting `api_org` are rejected client-side by `login`. Any opaque token format works. HF-style `hf_...` tokens are recommended so tooling that pattern-matches them still works.

Exceptions to "auth on every request":
- **LFS single-part PUT, multipart part PUTs, and the multipart completion POST carry no `authorization`** (section 7.5). Those URLs must be self-authenticating (presigned or HMAC in the query).
- The download GET to an off-origin location has no auth.
- Xet CAS calls use the Xet token, not the Hub token.

### 6.2 `GET /api/whoami-v2`: **MUST** for `hf auth login --token` and `hf auth whoami`

Client: `HfApi.whoami` / `_inner_whoami`; `_login.py::_validate_and_save_token`; `cli/auth.py::auth_whoami`; also `get_full_repo_name`, `hf env` and `duplicate_repo` (`["name"]`).
```json
{
  "type": "user",
  "id": "64b...",
  "name": "james",
  "fullname": "James Garside",
  "email": "james@example.com",
  "emailVerified": true,
  "canPay": false,
  "isPro": false,
  "avatarUrl": "https://registry.example/avatars/james.png",
  "orgs": [
    {"type": "org", "id": "65c...", "name": "bellatrix", "fullname": "Bellatrix", "roleInOrg": "admin"}
  ],
  "auth": {
    "type": "access_token",
    "accessToken": {"displayName": "laptop", "role": "write", "createdAt": "2026-10-01T00:00:00.000Z"}
  }
}
```

| Field | Requirement | Reader |
|---|---|---|
| `name` | **MUST** | login (`token_info["name"]`), `whoami` CLI, many namespace defaults |
| `orgs` (list, may be empty) with `orgs[].name` | **MUST** | `hf auth whoami` does `info["orgs"]` (KeyError) |
| `auth.accessToken.displayName` | **SHOULD** | becomes the stored token name; otherwise `oauth-{name}` |
| `auth.accessToken.role` | **SHOULD** (`read`, `write` or `fineGrained`) | logged "Token is valid (permission: write)" |
| `id` | **MAY** | sandbox features only |
| others (`fullname`, `email`, `canPay`, `isPro`, `avatarUrl`, `orgs[].roleInOrg`) | **MAY** | `_oauth.py` (Spaces OAuth) |

**Errors**: 401 for an invalid or missing token. The client rewrites a 401 as "Invalid user token." and a 429 as a whoami rate-limit message. The Hub returns `{"error":"Invalid username or password."}`.

### 6.3 Interactive `hf auth login` (no `--token`): **MAY**

When stdin is a TTY the user chooses between browser and paste. **When it is not a TTY, the browser (device code) flow is chosen automatically** (`_login.py::_prompt_login_method`). That flow calls:
- `POST {ENDPOINT}/oauth/device` form `client_id=<HF_DEVICE_CODE_OAUTH_CLIENT_ID>`, which expects `{device_code, user_code, verification_uri, verification_uri_complete?, interval?, expires_in?}` (`utils/_oauth_device.py::request_device_code`).
- `POST {ENDPOINT}/oauth/token` with `grant_type=urn:ietf:params:oauth:grant-type:device_code`, polled until `access_token` arrives. Errors `authorization_pending`, `slow_down`, `expired_token` and `access_denied` are RFC 8628 (`poll_device_token`).
- Later refresh: `POST /oauth/token` with `grant_type=refresh_token` (`refresh_access_token`), triggered from `get_token()` for stored OAuth tokens.

Document `hf auth login --token <t>` (or `HF_TOKEN`) as the supported path. Implementing RFC 8628 against our IdP is optional.

---

## 7. Upload path

### 7.0 Call sequence

`hf upload` (`cli/upload.py`) → `create_repo(exist_ok=True)` → [`repo_info(revision)` → `create_branch` if `RevisionNotFound`] → `upload_file` / `upload_folder`.

`upload_file` / `create_commit` (`hf_api.py::create_commit`):
1. If `README.md` is among the additions: `POST /api/validate-yaml` (7.4).
2. `POST /api/{type}s/{repo}/preupload/{rev}` in batches of 256 files (7.5).
3. Upload LFS-mode files: **Xet when `hf_xet` is available** (section 8), otherwise `POST .../info/lfs/objects/batch` then PUT, and optionally verify or multipart-complete (7.6).
4. Copies: `paths-info` on the destination and source; GET resolve for regular source files.
5. If every add/copy is a no-op (remote oid equals local oid): `repo_info` and return without committing.
6. `POST /api/{type}s/{repo}/commit/{rev}` with an NDJSON body (7.7).

`upload_folder` **without** hf_xet goes through `create_commit` (one commit). **With** hf_xet it uses `_upload_pipeline.py::pipelined_upload`: preupload in batches of 256, Xet uploads, **multiple commits** (adaptive 20–1000 files, "(part N)" messages, `retry_on_error=True` on the commit POST), `POST /api/{type}s/{repo}/discussions` (`create_pull_request`) for `create_pr`, and no `validate-yaml` call.

### 7.1 `POST /api/repos/create`: **MUST**

Client: `HfApi.create_repo`.
```json
{"name": "my-model", "organization": "bellatrix", "visibility": "private", "type": "dataset"}
```
- `organization`: `null` when `repo_id` has no `/`, in which case the server uses the token owner.
- `visibility`: `"private"` or `"public"`, only when the caller passed `private`/`visibility`. **v2.x no longer sends `"private": bool`**, but older clients do: **SHOULD** accept both.
- `type`: **omitted for models** (only set when `repo_type` is not `None`); `"dataset"`, `"space"` or `"kernel"` otherwise. Space keys (`sdk`, `hardware`, ...) and `resourceGroupId`/`region` can be ignored.

**Response 200**: `{"url": "https://registry.example/bellatrix/my-model"}`. Hub-style extras such as `name` and `id` are harmless. The client does `RepoUrl(d["url"], endpoint)`, which **MUST** parse as `{base}/[datasets/]{ns}/{name}`, with the host equal to the `HF_ENDPOINT` host (or `huggingface.co`, which the client rewrites to the endpoint via `fix_hf_endpoint_in_url`), and **two segments** (`utils/_hf_uris.py::_url_to_uri_body`).

**409 when the repo exists: the body MUST still contain `url`.** With `exist_ok=True` (always the case for `hf upload`) the client swallows the 409 and then executes `r.json()["url"]`, so a body without `url` raises KeyError:
```json
HTTP/1.1 409 Conflict
{"error": "You already created this model repo", "url": "https://registry.example/bellatrix/my-model"}
```
**MUST NOT** use the message `Cannot create repo: another conflicting operation is in progress` on a 409 unless the condition is transient: the client retries that one **in an unbounded loop**. On 401/402/403 with `exist_ok`, the client checks `repo_info` and returns the URL if the repo exists.

**SHOULD**: create an initial commit on `main` (the Hub adds `.gitattributes` with the default LFS patterns), so `main` resolves immediately and tree, info and snapshot calls succeed on a fresh repo.

### 7.2 `DELETE /api/repos/delete`: **SHOULD**
Body `{"name": "...", "organization": "..."|null, "type": "dataset"?}` (`HfApi.delete_repo`). 200. 404 + `RepoNotFound` is tolerated when `missing_ok=True`.

### 7.3 `PUT /api/{type}s/{repo}/settings`: **SHOULD**
Body `{"gated": "auto"|"manual"|false, "visibility": "private"|"public"}` (at least one key; `HfApi.update_repo_settings`). Older clients send `"private": bool`, so **SHOULD** accept it. 200, body ignored.

### 7.3b `POST /api/repos/move`: **MAY**
Body `{"fromRepo": "a/b", "toRepo": "c/d", "type": "model"}` (`HfApi.move_repo`). See section 10 for the redirects that should follow.

### 7.4 `POST /api/validate-yaml`: **MUST** when uploading `README.md` through `create_commit` / `upload_file`

Client: `hf_api.py::HfApi._validate_yaml`. Body `{"content": "<README text>", "repoType": "model"}`. **The client calls `response.json()` before checking the status**, so a 404 with a non-JSON body crashes with a JSON decode error, and a JSON 404 still raises `HfHubHTTPError` and aborts the commit.
```json
200 {"errors": [], "warnings": []}
200 {"errors": [], "warnings": [{"message": "empty or missing yaml metadata in repo card", "help": "https://..."}]}
400 {"errors": [{"message": "..."}], "warnings": []}   -> client raises ValueError("Invalid metadata in README.md")
```
Live Hub responses confirmed the first two. Minimal compliant implementation: always `200 {"errors":[],"warnings":[]}`, or parse the YAML front matter and report syntax errors.

### 7.5 `POST /api/{type}s/{repo}/preupload/{rev}[?create_pr=1]`: **MUST**

Client: `_commit_api.py::_fetch_upload_modes` (from `preupload_lfs_files` and `_upload_pipeline.py`). Sent through `http_backoff` (retries on 429/5xx).
```json
{
  "files": [
    {"path": "config.json", "sample": "<base64 of first 512 bytes>", "size": 665},
    {"path": "model.safetensors", "sample": "...", "size": 548105171}
  ],
  "gitIgnore": "<contents of .gitignore being uploaded, optional>"
}
```
**Response 200**:
```json
{
  "files": [
    {"path": "config.json", "uploadMode": "regular", "shouldIgnore": false, "oid": "10c66461e4c109db5a2196bff4bb59be30396ed8"},
    {"path": "model.safetensors", "uploadMode": "lfs", "shouldIgnore": false, "oid": null}
  ],
  "commitOid": "..."
}
```

| Field | Requirement |
|---|---|
| `files` | **MUST** be a list (`_validate_preupload_info`) |
| `files[].path` | **MUST**, string, echoing every requested path (the client indexes by path) |
| `files[].uploadMode` | **MUST** be `"lfs"` or `"regular"` |
| `files[].shouldIgnore` | **MUST** be present (`file["shouldIgnore"]`, KeyError). `true` means the file matches the server-side `.gitignore` and the client drops it from the commit |
| `files[].oid` | optional. Current remote content id of that path at `{rev}`: **sha256 for lfs-mode, git sha1 for regular-mode**. When equal to the local value (`CommitOperationAdd._local_oid`), the client treats the file as unchanged and skips it. `null` or absent means a new file |

Mode policy is server-defined. The client only forces size-0 files to `regular`. Recommended, Hub-like policy: `lfs` if the path matches an LFS rule in the repo's `.gitattributes`, **or** size ≥ 10 MiB, **or** the sample looks binary (contains NUL); otherwise `regular`. **Regular files are base64-inlined in the commit body and held in client memory**, so keep the regular threshold small. Section 8 explains why `regular` matters for Xet avoidance.

### 7.6 LFS batch and transfer (HTTP path, used only when Xet is not used)

#### `POST /{repo}.git/info/lfs/objects/batch` and `POST /datasets/{repo}.git/info/lfs/objects/batch`: **MUST** for upload without Xet

Client: `lfs.py::post_lfs_batch_info`, called from `_commit_api.py::_upload_files` in chunks of 256. Headers: `Accept: application/vnd.git-lfs+json`, `Content-Type: application/vnd.git-lfs+json`, plus `authorization` and `user-agent`.
```json
{
  "operation": "upload",
  "transfers": ["basic", "multipart"],
  "objects": [{"oid": "<sha256 hex>", "size": 548105171}],
  "hash_algo": "sha256",
  "ref": {"name": "main"}
}
```
`ref` is omitted when `create_pr=True` (`preupload_lfs_files` passes `revision=None`). `ref.name` is the **unquoted** revision.

**Response 200**:
```json
{
  "transfer": "basic",
  "objects": [
    {"oid": "<sha256 a>", "size": 1024,
     "actions": {
       "upload": {"href": "https://registry.example/lfs-upload/<sha256 a>?sig=..."},
       "verify": {"href": "https://registry.example/bellatrix/my-model.git/info/lfs/objects/verify", "header": {"Authorization": "ignored-by-hf-client"}}
     }},
    {"oid": "<sha256 b>", "size": 548105171},
    {"oid": "<sha256 c>", "size": 9,
     "error": {"code": 422, "message": "size mismatch"}}
  ]
}
```

| Rule | Source |
|---|---|
| `objects` **MUST** be a list ("Malformed response from server") | `post_lfs_batch_info` |
| Each object **MUST** have string `oid` and int `size` | `_validate_batch_actions`, `_validate_batch_error` |
| **No `actions` key means "already present, skip"** (dedup) | `_upload_lfs_files`, `lfs_upload` |
| `actions.upload.href` **MUST** be a string; `header`, if present, a dict | `_validate_lfs_action` |
| `error` objects **MUST** have int `code` and string `message`; any error aborts the upload with `ValueError` | `_validate_batch_error`, `_upload_files` |
| `transfer` is **ignored** in this code path (the returned value is discarded as `_`) | `_upload_files` |
| `href` strings containing `https://huggingface.co` are rewritten to `HF_ENDPOINT` | `utils/_http.py::fix_hf_endpoint_in_url` |

**Single-part vs multipart is decided only by `actions.upload.header.chunk_size`** (`lfs.py::lfs_upload`).

#### Single part (no `chunk_size`): `PUT {upload.href}`: **MUST**
`http_backoff("PUT", href, data=<file>)` (`lfs.py::_upload_single_part`): raw bytes, **no `authorization`, no headers from `upload.header`**, no Content-Type set. Retries on 5xx re-send the whole file. `Content-Length` is set: httpx2 treats `data=<file>` as raw content (with a DeprecationWarning) and sizes it with `tell`/`seek` (`httpx2/_content.py::encode_request` → `encode_content` → `_utils.py::peek_filelike_remaining_length`). Chunked encoding is used only for unsized iterables. **SHOULD** still accept chunked bodies from other clients. Return 2xx. **MUST** verify that the sha256 of the stored bytes equals `oid` before accepting it.

#### Multipart (`chunk_size` present): **SHOULD** (MUST if you want uploads over 5 GB to S3-style storage)
Upload action:
```json
"upload": {
  "href": "https://registry.example/lfs-multipart/complete?uploadId=...&sig=...",
  "header": {
    "chunk_size": "104857600",
    "00001": "https://minio.example/bucket/tmp/<oid>?partNumber=1&uploadId=...&X-Amz-Signature=...",
    "00002": "https://minio.example/bucket/tmp/<oid>?partNumber=2&uploadId=...&X-Amz-Signature=..."
  }
}
```
Client behaviour (`lfs.py::_upload_multi_part`, `_get_sorted_parts_urls`, `_upload_parts_iteratively`, `_get_completion_payload`):
1. `chunk_size` is parsed with `int()`; a non-integer raises `ValueError`.
2. Part URLs are every header key where `key.isdigit()`, sorted by `int(key)` (zero-padding is fine). **The count MUST equal `ceil(size / chunk_size)`**, otherwise `ValueError("Invalid server response to upload large LFS file")`.
3. Parts are uploaded **sequentially**: `PUT part_url` with exactly `chunk_size` bytes (the last part is shorter) and `Content-Length` set (a `SliceFileObj` sized via `tell`/`seek`). No auth, no extra headers. `http_backoff` retries 5xx.
4. Each part response **MUST** carry an `ETag` header (read case-insensitively as `etag`). An empty or missing value raises `ValueError`.
5. Completion: `POST {upload.href}` with **only** `Accept`/`Content-Type: application/vnd.git-lfs+json`. **No `authorization`, no user-agent.** Body:
```json
{"oid": "<sha256>", "parts": [{"partNumber": 1, "etag": "\"abc...\""}, {"partNumber": 2, "etag": "\"def...\""}]}
```
The ETag value is passed through verbatim, quotes included. That makes S3/MinIO `CompleteMultipartUpload` a direct mapping. The server **MUST** then verify size and sha256 (or trust the S3 checksum plus a background re-hash) before the object becomes committable. Return 2xx. Note that the `upload.href` is **the completion endpoint**, not an upload target.

The same `chunk_size` + numbered-URL convention is used by the git-lfs custom transfer agent `hf lfs-multipart-upload` (`cli/lfs.py::lfs_multipart_upload`).

#### Verify (optional): `POST {verify.href}`: **MAY**
Sent only when `actions.verify` is present (`lfs.py::lfs_upload`). Headers: `authorization: Bearer <hub token>` and `user-agent`; **`verify.header` is ignored**. Body `{"oid": "<sha256>", "size": N}`. Return 2xx. If you verify on PUT/complete, omit `verify`.

### 7.7 `POST /api/{type}s/{repo}/commit/{rev}[?create_pr=1][&hot_reload=1]`: **MUST**

Client: `_commit_api.py::_send_commit` and `_prepare_commit_payload`. Headers: `Content-Type: application/x-ndjson`, `authorization`. `{rev}` is `quote(rev, safe="")`. With the Xet pipeline, the request goes through `http_backoff` with retries, so **SHOULD** make commits idempotent where possible (`parentCommit` helps). Body: one JSON object per line, header first:
```
{"key": "header", "value": {"summary": "Upload model", "description": "", "parentCommit": "607a30d7..."}}
{"key": "file", "value": {"content": "eyJtb2RlbF90eXBlIjogImJlcnQifQ==", "path": "config.json", "encoding": "base64"}}
{"key": "lfsFile", "value": {"path": "model.safetensors", "algo": "sha256", "oid": "248dfc39...a707", "size": 548105171}}
{"key": "lfsFile", "value": {"path": "copy-of-weights.safetensors", "algo": "sha256", "oid": "248dfc39...a707"}}
{"key": "deletedFile", "value": {"path": "old.bin"}}
{"key": "deletedFolder", "value": {"path": "logs/"}}
```

| Line | Semantics |
|---|---|
| `header` | `summary` (non-empty, enforced client-side), `description` (may be `""`), optional `parentCommit` (5–40 hex). **SHOULD** reject with 412 or 409 when `parentCommit` is not the branch head. The client just raises `HfHubHTTPError`, so the exact code is our choice. |
| `file` | regular file; `content` is base64 of the full bytes. Also used for **copies of regular files** (the client downloads the source via GET resolve and re-sends it). |
| `lfsFile` | add a pointer to an already-uploaded object (LFS or Xet). **`size` is absent for copies** (`CommitOperationCopy` of an LFS file), so look the size up from the object store. **MUST** fail (4xx) if the object is unknown or not fully uploaded. |
| `deletedFile` | delete a file. If missing, **SHOULD** return 404 + `EntryNotFound` with a message containing `A file with this name doesn't exist`. The client matches that text to append its folder-vs-file hint (`create_commit`). |
| `deletedFolder` | recursive delete. The path **may end in `/`**, so normalise it. |
| ~~`copy`~~ | **Not sent by v2.2.0.** Copies are converted to `file`/`lfsFile` lines. Cross-repo LFS copies first call `POST /api/{src_type}s/{src}/lfs-files/duplicate`, which **requires a Xet hash**: the client raises `ValueError("file has no xet hash")` on a non-Xet server (`HfApi._duplicate_lfs_files`). Cross-repo LFS copy is therefore unsupported against a non-Xet registry. |

Paths are client-validated (no leading `/`, no `..`, no `.git/` component; `_commit_api.py::_validate_path_in_repo`). The server **MUST** re-validate.

`create_pr=1`: create the commit on a new `refs/pr/N` based on `{rev}` and open a PR record.

**Response 200**:
```json
{
  "success": true,
  "commitOid": "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b",
  "commitUrl": "https://registry.example/bellatrix/my-model/commit/9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b",
  "pullRequestUrl": "https://registry.example/bellatrix/my-model/discussions/1"
}
```

| Field | Requirement |
|---|---|
| `commitOid` | **MUST** |
| `commitUrl` | **MUST**. The client builds `RepoUrl(commit_url.split("/commit/")[0])`, which **must parse** as `{base}/[datasets/]{ns}/{name}` on the endpoint host (`hf_api.py::CommitInfo.__post_init__`) |
| `pullRequestUrl` | **MUST when `create_pr=1`** (direct indexing). It must match `.*/discussions/(\d+)`, from which the client derives `refs/pr/N` (`_parse_revision_from_pr_url`) |

Errors: 404 `RepoNotFound`, `RevisionNotFound` or `EntryNotFound` as above; 400 is surfaced as "Bad request for commit endpoint" (`BadRequestError`); 403 for no write access; 409/412 for a parent mismatch.

### 7.8 Other upload-adjacent endpoints
- `POST /api/{type}s/{repo}/discussions` with `{"title", "description", "pullRequest": true}`: used only by the Xet multi-commit pipeline for `create_pr` (`HfApi.create_pull_request`). **MAY**.
- `POST /api/{type}s/{repo}/lfs-files/duplicate`: Xet-only. **MAY**/never.
- `super_squash_history`, `list_lfs_files`, `permanently_delete_lfs_files`, discussions, webhooks, collections: **MAY**, not used by loaders.

---

## 8. Xet: how the client chooses and how to stay on plain HTTP

### 8.1 Client-side switch
`utils/_runtime.py::is_xet_available()` returns `not HF_HUB_DISABLE_XET and is_package_available("hf_xet")`. `HF_HUB_DISABLE_XET` is read once at import (`constants.py`) and accepts `1/ON/YES/TRUE`. `hf_xet>=1.6` is installed by default on x86_64, amd64, arm64 and aarch64 (`setup.py`).

### 8.2 Downloads: the server is in control
Xet download is used only if **both**:
1. `is_xet_available()`, **and**
2. the client holds Xet file data, which comes from exactly two places:
   - resolve response headers: `X-Xet-Hash` **plus** either `Link: <url>; rel="xet-auth"` or `X-Xet-Refresh-Route` (`utils/_xet.py::parse_xet_file_data_from_response`);
   - tree/paths-info entries with `xetHash` (`RepoFile.xet_hash`). These are used by `snapshot_download`'s tree cache to **skip the HEAD entirely** (`file_download.py::_xet_file_metadata_from_tree_cache`, which requires `xet_hash` + `lfs.sha256` + `lfs.size`) and by `HfFileSystem.get_file` (`_get_xet_file_data`, which builds the token URL `/api/{type}s/{repo}/xet-read-token/{rev}` itself).

If Xet data is present but hf_xet is missing or disabled, the client falls back to plain HTTP with a warning (`_download_to_tmp_and_move`). If hf_xet **is** present, there is **no fallback**: errors from `xet_get` or the token fetch (`GET <refresh_route>`, which expects `X-Xet-Cas-Url`, `X-Xet-Access-Token` and `X-Xet-Token-Expiration` headers; `utils/_xet.py::_fetch_xet_connection_info_with_url`) propagate.

**Server rules for a guaranteed HTTP download path (MUST)**
1. Never send `X-Xet-Hash`, `X-Xet-Refresh-Route`, `Link: rel="xet-auth"` or `rel="xet-reconstruction-info"` on resolve responses.
2. Never include `xetHash` in tree, paths-info or siblings entries.
3. Do not implement `/api/{type}s/{repo}/xet-read-token/{rev}` (404 is fine; it is never called if rules 1 and 2 hold).

With those rules, downloads work identically whether or not hf_xet is installed and regardless of `HF_HUB_DISABLE_XET`. Plain HTTP downloads are capped at 50 GB client-side (section 1.2).

### 8.3 Uploads: the client decides, with no server negotiation
- `_commit_api.py::_upload_files`: `if is_xet_available(): if not has_buffered_io_data: _upload_xet_files(...); return`. **The LFS batch endpoint is not called at all.** The old v0.x negotiation, where the client offered `transfers: [..., "xet"]` and the server picked, **no longer exists**: v2.2.0 sends `["basic", "multipart"]` only on the non-Xet path and ignores the response `transfer`.
- `hf_api.py::upload_folder`: `if is_xet_available(): return pipelined_upload(...)`, which is Xet-only for `lfs`-mode files.
- Xet upload: `hf_xet.XetSession.new_upload_commit(token_refresh_url=f"{endpoint}/api/{type}s/{repo}/xet-write-token/{rev}"[+"?create_pr=1"], ...)` (`utils/_xet.py::xet_connection_info_refresh_url`; with `create_pr` and no revision the URL literally ends in `/None`). It then talks to the CAS URL returned by that endpoint (`POST /v1/xorbs/default/{hash}`, `POST /v1/shards`; hub-docs `docs/xet/api.md`, `upload-protocol.md`). Then the normal commit is sent with `lfsFile` lines (sha256 oids). If the token endpoint 404s, the upload **fails**; huggingface_hub has no fallback code around `_upload_xet_files` or `pipelined_upload`.
- The only automatic HTTP fallback: `CommitOperationAdd` objects built from an open binary file object (`io.BufferedIOBase`), not from a path or bytes. Then the client logs "Uploading files as a binary IO buffer is not supported by Xet Storage. Falling back to HTTP upload."

**Options for our server (pick one)**

| Option | How | Trade-off |
|---|---|---|
| A. **Client config** (recommended baseline) | Users set `HF_HUB_DISABLE_XET=1` (or uninstall `hf_xet`) on machines that **upload**. Downloads need nothing. | Simple and correct; requires documentation and env management (CI, notebooks). |
| B. **`uploadMode: "regular"` for everything** | Preupload returns `regular` for all files, so the client never enters the Xet or LFS path. Content is base64-inlined into the NDJSON commit. | Works with hf_xet installed and zero client config, but the **whole file is read into memory and base64'd** (`CommitOperationAdd.b64content`, and `_send_commit` builds the whole body in memory), giving about 1.33× file size of RAM and one giant POST. Viable only for small files (a cut-off of a few hundred MB at most). The pipeline batches regular files at 100 MiB per commit (`REGULAR_CONTENT_BYTES_BUDGET`), but a single large file still goes in one request. Also, the git repo then stores those files as regular blobs, so ETags and the tree must use git sha1, not sha256. |
| C. **Hybrid** | `regular` below a threshold (say 50 MiB); `lfs` above, which requires A for big files. | Small-file uploads work out of the box; large ones fail clearly at `xet-write-token` unless Xet is disabled. Return a helpful 4xx JSON there: `{"error": "Xet is not supported by this registry; set HF_HUB_DISABLE_XET=1"}`. The client surfaces the message via `X-Error-Message` or the body. Whether hf_xet relays the body text is unverified (Rust side). |
| D. **Implement Xet** | Token endpoints (`GET /api/{type}s/{repo}/xet-{read,write}-token/{rev}`, answered with headers `X-Xet-Cas-Url`, `X-Xet-Access-Token`, `X-Xet-Token-Expiration` **and** JSON `{"accessToken","exp","casUrl"}` per hub-docs `docs/xet/auth.md`) plus a CAS server (reconstruction, xorbs, shards, global dedup; `docs/xet/api.md`, `shard.md`, `xorb.md`). | Large effort; out of scope for v1. Also lifts the 50 GB download cap. |

**Recommendation**: A+C for v1. Make the 4xx on `xet-write-token` and `xet-read-token` explicit with `X-Error-Message: Xet storage is not supported by this registry. Set HF_HUB_DISABLE_XET=1.` (any 4xx works; the client never sees a Xet hash for downloads).

---

## 9. Errors

### 9.1 Mapping in `utils/_http.py::hf_raise_for_status`
3xx never raises. For 4xx/5xx the checks run **in this order**:

| # | Condition | Exception |
|---|---|---|
| 1 | `X-Error-Code: RevisionNotFound` | `RevisionNotFoundError` |
| 2 | `X-Error-Code: EntryNotFound` | `RemoteEntryNotFoundError` (subclass of `EntryNotFoundError` and `HfHubHTTPError`) |
| 3 | `X-Error-Code: GatedRepo` | `GatedRepoError` (subclass of `RepositoryNotFoundError`) |
| 4 | `X-Error-Message` **exactly** `Access to this resource is disabled.` | `DisabledRepoError` |
| 5 | `X-Error-Code: RepoNotFound` and URL matches `/api/buckets/` | `BucketNotFoundError` (n/a) |
| 6 | 404 and URL matches `/api/jobs/...` | `JobNotFoundError` (n/a) |
| 7 | `X-Error-Code: RepoNotFound`, **or** (401 and `X-Error-Message != "Invalid credentials in Authorization header"` and URL matches `REPO_API_REGEX`) | `RepositoryNotFoundError` |
| 8 | 400 | `BadRequestError` |
| 9 | 403 | `HfHubHTTPError` ("Forbidden: {X-Error-Message}") |
| 10 | 429 | `HfHubHTTPError`, with rate-limit details if `RateLimit: "<type>";r=0;t=<s>` is present |
| 11 | 416 | `HfHubHTTPError` (HfFileSystem streaming treats it as EOF) |
| 12 | anything else | `HfHubHTTPError` |

**Important**: `REPO_API_REGEX` begins with `^https://` (`utils/_http.py`). The Hub itself returns **401 without `X-Error-Code`** for missing or private repos (live: `HEAD /nobody-xyz/nothing-xyz/resolve/main/config.json` gives 401 with only `x-error-message: Invalid username or password.`). The client only maps that to `RepositoryNotFoundError` via the https URL heuristic. **Our server MUST always send `X-Error-Code` explicitly**, which works over plain `http://` too.

The message comes from `X-Error-Message` plus the JSON body (`_format`): `{"error": "msg"}`, `{"error": ["a","b"]}`, `{"error": "code", "error_description": "..."}`, or `{"errors": [{"message": "..."}]}`. Non-JSON, non-HTML text bodies are appended raw.

### 9.2 Server error contract (MUST)
```
HTTP/1.1 404 Not Found
Content-Type: application/json; charset=utf-8
X-Error-Code: EntryNotFound
X-Error-Message: config.json does not exist on "main"
X-Repo-Commit: 607a30d783dfa663caf39e06633721c8d4cfcd7e      (resolve, when the revision resolved)
X-Request-Id: 01J...

{"error": "config.json does not exist on \"main\""}
```

| Situation | Status | `X-Error-Code` |
|---|---|---|
| Repo does not exist, or private and caller lacks read | 404 (401 if no token presented) | `RepoNotFound` |
| Revision unknown (`/revision/x`, `/resolve/x/..`, `/tree/x`, commit to an unknown branch) | 404 | `RevisionNotFound` |
| Path unknown (resolve, tree on a missing or file path, deletedFile on a missing file) | 404 | `EntryNotFound` |
| Gated, no access | 403 | `GatedRepo` |
| Disabled repo | any 4xx (the client keys only on the message; the Hub's status code was not verified) | (none) + `X-Error-Message: Access to this resource is disabled.` |
| Bad token | 401 | none; `X-Error-Message: Invalid credentials in Authorization header` keeps the client from mislabelling it as RepoNotFound |
| No write permission | 403 | none |
| Repo exists on create | 409 | none; body **must include `url`** |
| Rate limit | 429 + `Retry-After: <s>` or `RateLimit` / `RateLimit-Policy` | none |

Retry behaviour: the client retries 408, 429, 500, 502, 503 and 504 with exponential backoff (1→8 s, 5 tries) on `http_backoff` calls (`_DEFAULT_RETRY_ON_STATUS_CODES`), honouring `Retry-After` (integer seconds only) and `RateLimit` `t=` when `r=0`. `get_session().get/post` calls (model_info, commit without the pipeline, create_repo, whoami) are **not** retried.

---

## 10. Renamed and moved repos: **SHOULD**

Hub behaviour (live cassette `tests/cassettes/InferenceApiTest.test_simple_inference.yaml`): `GET /api/models/bert-base-uncased` returns **`307 Temporary Redirect`** with a **relative** `Location: /api/models/google-bert/bert-base-uncased`. The huggingface_hub test `test_file_download.py::test_download_regular_file_from_private_renamed_repo` shows `hf_hub_download(old_id)` working after `move_repo`, including private repos.

Client mechanics:
- API GET/POST (`get_session`, `http_backoff`): followed automatically. 307/308 preserve method and body; 301/302 turn a POST into a GET under httpx semantics. **Use 307.**
- Resolve HEAD: a same-origin redirect is followed **with auth kept** (`_is_same_or_hub_host`), and metadata headers are read from the final response, so the new location must return the full header set.
- The cache keeps using the **old** repo folder name (keyed by the requested id), which is harmless.

Implementation: keep a `redirects(old_type, old_id) → new_id` table written by `/api/repos/move`. For any path under `/{old}/...`, `/datasets/{old}/...` or `/api/{type}s/{old}/...`, rewrite the prefix and return `307` with a relative `Location`, keeping the rest of the path and the query. Apply authorisation **after** redirecting, so a private moved repo does not leak existence to unauthorised callers. Optionally, single-segment canonical ids can redirect the same way.

---

## 11. Downstream library requirements (transformers, diffusers, sentence-transformers, datasets, vLLM)

Source: a survey of each library's default branch, read 2026-10-13: transformers `8741ef5a`, diffusers `916a5791`, sentence-transformers `1f882da8` (now `huggingface/sentence-transformers`), datasets `9e7496a4` (2026-10-06), vllm `de8291e2`. I re-checked the transformers auto-conversion path by hand. The other citations are from the survey, so treat them as "read once, not re-verified line by line".

### 11.1 Common requirements (all five libraries)

| # | Requirement | Why | Tag |
|---|---|---|---|
| 1 | **Missing optional file → `404` + `X-Error-Code: EntryNotFound` + `X-Repo-Commit`**, on both HEAD and GET `/resolve/` | transformers `utils/hub.py::cached_files` returns `None` only on `EntryNotFoundError` (with `_raise_exceptions_for_missing_entries=False`). A code-less 404 becomes `HfHubHTTPError` → `OSError` and aborts `from_pretrained`. sentence-transformers `util/file_io.py::load_file_path` treats only `EntryNotFoundError`/`LocalEntryNotFoundError`/`HFValidationError` as missing. datasets `utils/file_utils.py::cached_path` maps only the coded errors to `FileNotFoundError`. vllm `download_safetensors_index_file_from_hf` catches only `EntryNotFoundError`/`LocalEntryNotFoundError`. | **MUST** |
| 2 | `GET /api/{models,datasets}/{repo}/revision/{rev}` returns `sha` | transformers, diffusers, sentence-transformers and vllm all call `HfApi.resolve_revision` first. datasets calls `dataset_info(..., timeout=100.0).sha`. | **MUST** |
| 3 | Unknown revision → `RevisionNotFound`; unknown repo → `RepoNotFound`. **Never** return either for a missing *path* | transformers `list_repo_templates` (tree of `additional_chat_templates/`) re-raises `RepositoryNotFoundError`/`RevisionNotFoundError`/`GatedRepoError` but swallows other HTTP errors. A missing folder must be `404 EntryNotFound` (or `200 []`). | **MUST** |
| 4 | Range → 206 | datasets streaming (`HfFileSystem.open`) and vllm `get_safetensors_metadata` / `parse_safetensors_file_metadata`. | **MUST** |
| 5 | Recursive tree with `type`, `path` and `size` | vllm `repo_utils.file_exists` is implemented as "name in `list_repo_files`", so an incomplete recursive listing makes files invisible. datasets `data_files.resolve_pattern` uses `HfFileSystem.glob`. | **MUST** |

### 11.2 transformers (`from_pretrained`, `AutoTokenizer`, `AutoProcessor`, `pipeline`)

- **Calls**: `resolve_revision`, then `cached_files` → `try_to_load_from_cache`, then `hf_hub_download` (single file) or `snapshot_download(allow_patterns=...)` (several). Shards go through `get_checkpoint_shard_files`. `list_repo_tree(path_in_repo="additional_chat_templates", recursive=False)`. Tokenizer `list_repo_files` (errors swallowed). `model_info().tags` in `is_base_mistral` (swallowed). `model_info().pipeline_tag` and `.library_name` in `pipelines.get_task`, only when `pipeline()` has no task.
- **`has_file`** issues its own `HEAD {endpoint}/{repo}/resolve/{rev}/{file}` with `follow_redirects=False`. **Any 2xx or 3xx counts as present.** `EntryNotFound` means absent.
- **Optional probes** (each one a HEAD that must 404 cleanly): `adapter_config.json`, `config.json`, `generation_config.json`, `model.safetensors`, `model.safetensors.index.json`, `pytorch_model.bin`, `pytorch_model.bin.index.json` (all variant-suffixed when a variant is set), `tokenizer_config.json`, `tokenizer.json`, `special_tokens_map.json`, `added_tokens.json`, `chat_template.jinja`, `chat_template.json`, `processor_config.json`, `preprocessor_config.json`, `video_preprocessor_config.json`, `tokenizer.model`/`spm.model`, and the `additional_chat_templates/` listing.
- **Safetensors auto-conversion bot** (`safetensors_conversion.py`, triggered from `modeling_utils.py` when only `.bin` weights exist and `has_file(model.safetensors[.index.json])` is false, or when `use_safetensors=True` and `revision == "main"`):
  1. `model_info().private`.
  2. `list_repo_commits(...)[0]`.
  3. `get_repo_discussions(...)`.
  4. `POST https://safetensors-convert.hf.space/call/run` with `{"data": [model_id, private, token]}`. **This URL is hardcoded and ignores `HF_ENDPOINT`**, and it sends the explicitly passed token (`kwargs.get("token")`) and the repo id to Hugging Face.

  Guard (MUST, for privacy): either have clients set `DISABLE_SAFETENSORS_CONVERSION=1`, or make the step that precedes the POST fail. Answering `GET /api/{type}s/{repo}/discussions` with an error (for example `404`) makes `previous_pr` raise before `spawn_conversion`. **Do not return an empty discussions list.** When it runs in a background thread, the exception is swallowed (survey finding).
- **Metadata fields read**: `sha`, `tags`, `pipeline_tag`, `library_name`, `private`.

### 11.3 diffusers

- `DiffusionPipeline.download` (`pipelines/pipeline_utils.py`) calls `model_info(repo, revision)` and **builds the whole download plan from `siblings[].rfilename`**. That list must be **complete and recursive**, including `unet/...`, `vae/...` and the other sub-model folders. It then calls `hf_hub_download(model_index.json)` and `snapshot_download(allow_patterns, ignore_patterns)`. **MUST**: always return full recursive `siblings` from `model_info` (no `blobs=true` needed).
- `utils/hub_utils.py::_get_checkpoint_shard_files` checks each shard against `model_info().siblings` by substring, then calls `snapshot_download`. `lora_base` lists siblings when no `weight_name` is given.
- Probes: `model_index.json`, `config.json`, `scheduler_config.json`, `diffusion_pytorch_model[.variant].safetensors[.index.json]`, `.bin` fallbacks, `README.md`. Any non-2xx is treated as "no index" (`_fetch_index_file` catches `EntryNotFoundError` and `EnvironmentError`).
- Community pipelines download from the dataset repo `diffusers/community-pipelines-mirror`, and also call `pypi.org`. Neither applies to a private registry; **MAY** mirror the dataset.

### 11.4 sentence-transformers

- `resolve_revision` (a `RevisionNotFoundError` is fatal), then `hf_hub_download(library_name="sentence-transformers")` for `modules.json`, `config_sentence_transformers.json`, `README.md`, per-module `sentence_bert_config.json` (plus six legacy `sentence_*_config.json` names), `router_config.json`, `model.safetensors`, then `pytorch_model.bin`. Also `snapshot_download(allow_patterns="{sub}/**")` and `list_repo_files` (ONNX/OpenVINO backends only). The Transformer module then delegates to transformers with `subfolder=`.
- Strict requirement: optional files **must** produce `EntryNotFoundError` (11.1 #1). No repo metadata fields are used.

### 11.5 datasets (`load_dataset("ns/name")`)

- `load.py::dataset_module_factory`:
  1. `hf_hub_download("README.md", repo_type="dataset", revision)`. The **commit sha is taken from the snapshot directory name**, which comes from `X-Repo-Commit`.
  2. If `README.md` is `EntryNotFound`: `dataset_info(revision, timeout=100.0).sha`.
  3. Probe `{name}.py`, which **must** be `EntryNotFound`; an existing script makes loading fail ("scripts no longer supported").
  4. `HubDatasetModuleFactory.get_module` reads `.huggingface.yaml` and `dataset_infos.json` (both optional, coded 404 required), then resolves data files with `HfFileSystem.glob("hf://datasets/{repo}@{sha}/...", detail=True, expand_info=False)` → recursive tree with `type`/`path`/`size`.
- Downloads go through `HfFileSystem.resolve_path` (→ `repo_info`), then `hf_hub_download`. Streaming goes through `HfFileSystem.open` with Range reads (MUST 206; otherwise `NonStreamableDatasetError`).
- `GatedRepoError` is inspected by `response.status_code`: **401 means "log in", 403 means "request access"**.
- **External calls**, all errors swallowed, so safe to ignore:
  - `GET {HF_ENDPOINT with "://" replaced by "://datasets-server."}/info?dataset=...` with a 100 s timeout (`utils/_dataset_viewer.py`), for default-branch loads with no `data_files`/`data_dir`. If a `datasets-server.<our host>` DNS name exists it must answer quickly. Otherwise make sure the name does **not** resolve, so the lookup fails fast instead of waiting 100 s.
  - `HEAD https://s3.amazonaws.com/datasets.huggingface.co/...` (download counter, 3 s timeout; disable with `HF_UPDATE_DOWNLOAD_COUNTS=0`).
- The `refs/convert/parquet` ref is only used by a module factory that nothing instantiates on the default load path (survey). **MAY** implement `converts` refs.

### 11.6 vLLM

- `transformers_utils/repo_utils.py`: `resolve_revision`, a cached and retried `list_repo_files`, `file_exists` (implemented over `list_repo_files`), `file_or_path_exists` (cache, then list), `snapshot_download(ignore_patterns=weights/media)` for non-weight files, and `hf_hub_download` for single configs (lenient: any `HfHubHTTPError` gives `None`).
- `model_executor/model_loader/weight_utils.py::download_weights_from_hf`: `HfFileSystem.ls(repo[/subfolder], detail=False, revision)` (non-recursive tree), then picks a pattern. If `model.safetensors.index.json` exists, it is downloaded and only the shards in `weight_map` are fetched. Then `snapshot_download(allow_patterns)`. Also `snapshot_download("*.json")` for quant and sparse-attention configs.
- `transformers_utils/config.py`: `HfApi.get_safetensors_metadata` (HEAD `model.safetensors`, then the index, then Range reads of the safetensors headers) and `parse_safetensors_file_metadata`. `NotASafetensorsRepoError` is fatal.
- vLLM sets `HF_XET_HIGH_PERFORMANCE=True` (`weight_utils.enable_xet_high_performance`). That is irrelevant when the server never advertises Xet. No `model_info` calls were found.

### 11.7 HfFileSystem (`hf://`, used by datasets, pandas, polars, duckdb)

Requirements already listed above, gathered here (`hf_file_system.py`):
- `resolve_path` → `repo_info(revision)`, which must distinguish `RepoNotFound` from `RevisionNotFound`.
- `ls`/`glob`/`find` → tree, where a 404 `EntryNotFound` on a file path triggers a parent listing.
- `info` → parent tree or `paths-info` (missing paths omitted).
- `open().read()` → GET with `Range: bytes=a-b` (206 mandatory), plus streaming GET with `Range: bytes=n-` and 416 at EOF.
- `get_file` → GET with `expected_size` taken from the tree `size`.
- `modified()` → `expand=true` `lastCommit`, or `list_repo_commits`.
- **Writes** (`open("w")`, `put`, `rm`, `cp`) go through `upload_file` / `create_commit`, with the same Xet caveat (section 8).

---

## 12. `git clone` / `git push`

> Note: this research scoped git as later work. Hangar later chose full git support in v1.0; see `docs/spec/04-git.md`.

What full git compatibility would need, for scoping:
- **Smart HTTP** on `{base}/{ns}/{name}` and `{base}/datasets/{ns}/{name}` (with or without `.git`): `GET /info/refs?service=git-upload-pack` and `POST /git-upload-pack` for clone and fetch; `GET /info/refs?service=git-receive-pack` and `POST /git-receive-pack` for push. Basic auth with username = anything and password = token (git credential helpers; `hf auth login --add-to-git-credential` stores the token there, `_login.py::_set_active_token`).
- A real git object store, so commits made through the NDJSON API are actual git commits. Then the `oid`/sha values in this contract are real git ids, and clones match API listings.
- **git-lfs** server at `{repo}.git/info/lfs`: the same `objects/batch` endpoint as 7.6, plus `operation: "download"` (actions.download.href, presigned) and optional `locks` API (git-lfs probes `/locks/verify` on push and tolerates 404). `.gitattributes` LFS rules decide pointers (`version https://git-lfs.github.com/spec/v1\noid sha256:<hex>\nsize <n>\n`, which also gives the 134-ish byte `pointerSize`).
- Large pushes over 5 GB through the custom transfer agent `hf lfs-multipart-upload` (`cli/lfs.py`), which uses the multipart convention in 7.6.

If the storage layer is designed git-native from day one (commits, trees and blobs addressed by git sha1, LFS pointers for large files), smart HTTP can be added later with go-git without changing this API contract.

---

## 13. Prioritised checklist

### MUST: download-only compatibility (`hf_hub_download`, `snapshot_download`, `from_pretrained`, vLLM, HfFileSystem/`datasets` reads)
- [ ] Route models at `/{ns}/{name}/...` and datasets at `/datasets/{ns}/{name}/...` for resolve, and `/api/models/...` / `/api/datasets/...` for the API. Route on the **escaped path** and decode `%2F` in revision and tree-path segments.
- [ ] `HEAD` and `GET /resolve/{rev}/{path}` for branch, tag, full sha and `refs/pr/N`: `X-Repo-Commit` (full sha), quoted `ETag` + `X-Linked-Etag` (sha256 for LFS, git sha1 otherwise), exact `Content-Length` / `X-Linked-Size`, no `Content-Encoding`, answered within 10 s from stored metadata.
- [ ] If redirecting blobs: off-origin, presigned URL, metadata headers on the 3xx. No same-origin redirect unless the target re-emits all headers.
- [ ] `Range` → `206` with a numeric `Content-Range` (`bytes a-b/total`) on resolve and on any blob URL; 416 past EOF.
- [ ] `GET /api/{models|datasets}/{repo}` and `/revision/{rev}`: `id`, `sha` (full), `siblings[{rfilename}]` (+ `size`, `blobId`, `lfs{sha256,size,pointerSize}` with `blobs=true`), dates in `...Z`, `private`, `gated:false`, `disabled:false`. No `xetHash`, `transformersInfo` with unknown keys, `inferenceProviderMapping` or `evalResults`.
- [ ] `GET /api/{type}s/{repo}/tree/{rev}[/{path}]?recursive=&expand=` with entries `{type, oid, size, path, lfs{oid,size,pointerSize}}`, real sizes, directories included in recursive output, absolute `Link: <...>; rel="next"` on our public base URL, and 404 `EntryNotFound` for missing or file paths.
- [ ] `POST /api/{type}s/{repo}/paths-info/{rev}` (form with repeated `paths`, plus JSON): existing entries only.
- [ ] Error headers on every 4xx: `X-Error-Code` ∈ {`RepoNotFound`, `RevisionNotFound`, `EntryNotFound`, `GatedRepo`}, `X-Error-Message`, JSON `{"error": "..."}`; `X-Repo-Commit` on resolve `EntryNotFound`.
- [ ] Bearer token auth (anonymous read for public repos), with `401` + `X-Error-Message: Invalid credentials in Authorization header` for bad tokens.
- [ ] **Never** emit `X-Xet-Hash`, `Link rel=xet-auth`, `X-Xet-Refresh-Route` or `xetHash`.
- [ ] Booleans in query and form parsed case-insensitively (`true/false/True/False/1/0`); `expand` and `expand[]` both accepted.
- [ ] All JSON datetimes as UTC `YYYY-MM-DDTHH:MM:SS.sssZ`.
- [ ] Immutable commits: same sha means same tree and same bytes.
- [ ] `model_info` always returns **complete recursive `siblings`** (diffusers builds its download plan from it).
- [ ] `GET /api/{type}s/{repo}/discussions` returns an **error** (not `[]`) until discussions exist, so transformers' safetensors auto-conversion never POSTs the repo id and token to `safetensors-convert.hf.space`. Also document `DISABLE_SAFETENSORS_CONVERSION=1`.
- [ ] `datasets-server.<our host>` must not resolve, or must answer fast (datasets calls `/info` with a 100 s timeout).

### MUST: upload compatibility (`hf upload`, `upload_file`, `upload_folder`, `create_commit`, `push_to_hub`)
- [ ] `GET /api/whoami-v2` with `name`, `orgs[]` (`name`), `auth.accessToken.{role,displayName}`.
- [ ] `POST /api/repos/create` (accepts `visibility` **and** legacy `private`; `type` absent means model) → `{"url": "{base}/[datasets/]{ns}/{name}"}`. **409 includes `url`**. Initial commit on `main`.
- [ ] `POST /api/validate-yaml` → `200 {"errors":[],"warnings":[]}` (JSON always).
- [ ] `POST /api/{type}s/{repo}/preupload/{rev}` → `files[{path, uploadMode, shouldIgnore, oid?}]`, with a server-side lfs/regular policy and correct `oid` semantics (sha256 for lfs, git sha1 for regular).
- [ ] `POST /{repo}.git/info/lfs/objects/batch` (models and `datasets/` prefix): basic transfer, dedup by omitting `actions`, presigned `upload.href` (the PUT has no auth), sha256 verification of stored bytes.
- [ ] `POST /api/{type}s/{repo}/commit/{rev}` NDJSON: `header`, `file`, `lfsFile` (with and without `size`), `deletedFile`, `deletedFolder` (trailing `/`); `parentCommit` check; response `{commitOid, commitUrl}` (+ `pullRequestUrl` for `create_pr=1`) with a parseable `commitUrl`.
- [ ] `GET /api/{type}s/{repo}/revision/{rev}` returns `RevisionNotFound` (not `RepoNotFound`) for a missing branch; `POST .../branch/{b}` with 409 on exists (`hf upload --revision`).
- [ ] **Xet policy shipped and documented**: `HF_HUB_DISABLE_XET=1` required for uploaders, plus optional `regular` mode for small files, plus an explicit 4xx with a helpful message on `/xet-write-token/*` and `/xet-read-token/*`.

### SHOULD
- [ ] LFS **multipart** (`chunk_size` + numbered part URLs mapped to MinIO/S3 multipart; completion POST without auth; ETag per part) for files over 5 GB.
- [ ] 307 redirects for moved repos (`/api/repos/move`), applied to resolve, API and LFS paths.
- [ ] `GET /api/{type}s/{repo}/refs` (`branches`, `converts`, `tags`, `pullRequests`), `GET .../commits/{rev}` (paginated, `expand[]=formatted`).
- [ ] `expand=true` tree entries with `lastCommit{id,title,date}` (HfFileSystem `modified()`, `expand_info`).
- [ ] `GET /api/models` and `/api/datasets` search with `search`, `author`, `filter`, `sort`, `limit`, `full`, `cardData`, `config`, `expand`, cursor pagination; tolerate legacy `direction`.
- [ ] `DELETE /api/repos/delete`, `PUT /api/{type}s/{repo}/settings` (`visibility`/`private`, `gated`).
- [ ] `cardData`, `tags`, `library_name`, `pipeline_tag`, `config`, `author` in repo info (extracted from README.md front matter and config.json).
- [ ] `create_pr=1` on preupload and commit → `refs/pr/N` + `pullRequestUrl` `.../discussions/N`.
- [ ] `X-Request-Id` on every response; `Accept-Ranges: bytes`; `Content-Disposition`.
- [ ] `RateLimit` / `Retry-After` on 429.
- [ ] Accept unencoded special refs (`refs/pr/N`, `refs/convert/x`) in resolve and tree paths.

### MAY
- [ ] `POST /api/{type}s/{repo}/tag/{rev}`, `DELETE .../tag/{tag}`, `DELETE .../branch/{b}`.
- [ ] LFS `verify` action (`POST` with Bearer auth, `{oid,size}`).
- [ ] `securityStatus` / `securityFileStatus` (if sent, it must be complete).
- [ ] `safetensors` `{parameters,total}` and `gguf` metadata in model info; `usedStorage`, `downloads`, `likes`.
- [ ] Device-code OAuth (`/oauth/device`, `/oauth/token`) for interactive `hf auth login`.
- [ ] Discussions / PR API (`/api/{type}s/{repo}/discussions`, needed by the Xet pipeline's `create_pr`).
- [ ] Single-segment canonical id redirects.
- [ ] Hacky over-50 GB download support (off-origin 302 without `X-Linked-Size`).
- [ ] Xet CAS + token endpoints (removes the `HF_HUB_DISABLE_XET` requirement and the 50 GB cap).
- [ ] git smart HTTP + git-lfs download (section 12).
