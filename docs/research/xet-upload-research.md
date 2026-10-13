# Hangar: Xet upload (write) path, server contract and reassembly

Research date: 2026-10-13. Audience: whoever implements Hangar's CAS write path in Go.

## Sources and versions

| Source | Version read |
|---|---|
| `huggingface/hub-docs` `docs/xet/*.md` | `main` on 2026-10-13 (api, auth, upload-protocol, xorb, shard, hashing, chunking, deduplication, file-reconstruction, file-id) |
| `huggingface/xet-core` | `main` @ `e922f742` (2026-10-12). This is 3 commits after release `v1.7.0` (2026-10-06). The 3 commits add unused "xorb upload grant" types, report `shard_hash` in the shard response, and fix session poisoning. None of them changes the wire path below. |
| `huggingface_hub` | 2.2.0 (local copy in `scratchpad/hfsrc/hh`). It requires `hf-xet>=1.6.0,<2.0.0` (from the PyPI metadata), so Hangar must work with hf_xet 1.6.x and 1.7.x. I diffed `v1.6.0...v1.7.0`: 26 commits, mostly telemetry. One commit (`55823e47`) makes shard bytes depend only on content. None of them changes the endpoints or formats described here. |
| Licences | xet-core: Apache-2.0. hub-docs: Apache-2.0 (both from the GitHub API). |

Paths below are relative to the repo root: `xet-core/...` or `hub-docs/docs/xet/...`. Labels used:

- **[SRC]**: verified in code.
- **[DOC]**: from the spec docs only.
- **[UNVERIFIED]**: inference that I did not confirm.

**Doc/source mismatches found.** Wherever these conflict, follow the source.

1. Xorbs are uploaded **without** the footer. The docs never mention the footer.
2. The xorb "LZ4" is the **LZ4 frame format**.
3. The global-dedup query is sent with prefix `default`, not the `default-merkledb` the docs specify.
4. The hex inside merkle internal nodes is the **byte-swapped "API string" form**.
5. Telemetry goes to `/v1/telemetry` at the **root** of the CAS host.

---

## 1. Token endpoints

### 1.1 Who calls `xet-write-token`, and how

- **The URL is built by** `huggingface_hub/utils/_xet.py::xet_connection_info_refresh_url`:
  - It returns `f"{endpoint}/api/{repo_type}s/{repo_id}/xet-{read|write}-token"`, then appends `f"/{revision}"` for every repo type except buckets.
  - A Python `None` revision is formatted literally as `/None`. The source comment says the server "will return a token for PR refs".
  - Test cases are in `tests/test_xet_utils.py`: `.../xet-write-token/main` and `.../xet-write-token/None`. [SRC]
- **Upload callers:**
  - `_upload_pipeline.py` (around line 421): `revision = quote(revision or "main", safe="")`.
  - `_commit_api.py::_upload_xet_files` (around line 627) uses the same pattern.
  - Both append `?create_pr=1` when `create_pr=True`. The comment says "the final ref is not known in advance: `?create_pr=1` makes the server grant a token valid for PR refs". [SRC]
- **Revision encoding.** The revision is percent-encoded with `safe=""`, so `refs/pr/3` arrives as `refs%2Fpr%2F3`.
  - Route on the raw/escaped path. Go's `r.URL.Path` decodes `%2F` and would split the segment.
  - The `/None` case is only reachable from callers that pass `revision=None` without defaulting it. In the 2.2.0 create-commit paths, `revision` defaults to `main`. Accept `None` anyway and treat it as "default branch / any PR ref".
- **What is passed to hf_xet:**

  ```python
  session.new_upload_commit(
      token_refresh_url=refresh_url,
      token_refresh_headers=headers,                    # full Hub headers incl. Authorization: Bearer hf_...
      custom_headers=xet_headers_without_auth(headers), # same minus Authorization, sent on every CAS request
      progress_callback=...)
  ```

  [SRC] `_commit_api.py`, `_upload_pipeline.py`, `utils/_xet.py::xet_headers_without_auth`.

### 1.2 How hf_xet uses the endpoint

- **The commit is built in** `xet-core/xet_pkg/src/xet_session/common.rs::create_translator_config` [SRC]:
  - hf_xet is given a refresh URL but no endpoint. It therefore calls the refresh URL **once, eagerly**, through `DirectRefreshRouteTokenRefresher::get_cas_jwt()`.
  - It takes `casUrl` from that response as the CAS endpoint for the whole commit, and `accessToken`/`exp` as the initial token.
- **The refresh request** (`xet-core/xet_client/src/cas_client/auth.rs::DirectRefreshRouteTokenRefresher::get_cas_jwt`) [SRC]:
  - It is a plain `GET refresh_url` carrying `token_refresh_headers` as default headers.
  - It is wrapped in `RetryWrapper` (`run_and_extract_json`).
  - **The response must be a JSON body.** It is deserialised into `xet_client/src/hub_client/types.rs::CasJWTInfo` with `#[serde(rename_all = "camelCase")]`:

    ```json
    {"accessToken": "<string>", "exp": <u64 unix seconds>, "casUrl": "<string>"}
    ```

  - The retry wrapper treats a JSON decode failure as retryable. guilt/xet-server `docs/PROTOCOL.md` §6 reports that a headers-only, empty-body response makes `hf upload` hang in retries. [SRC for the struct; third-party report for the hang]
- **Expiry and refresh** (`auth.rs::TokenProvider`, `common/http_client.rs::AuthMiddleware`) [SRC]:
  - Before **every** CAS request, the middleware calls `get_valid_token()`. That function refreshes when `exp <= now + 30s` (`REFRESH_BUFFER_SEC = 30`).
  - A refresh updates only the token and `exp`. **The `casUrl` from later refreshes is ignored**; the endpoint is fixed for the commit's lifetime.
  - **A 401 from CAS is not retried and does not trigger a refresh.** `retry_wrapper.rs::classify` retries only 5xx, 408 and 429. So `exp` must be truthful, and the CAS must accept a token right up to `exp` (allow some clock skew).
  - The token is checked when a request starts, so a long shard upload does not need the token to outlive the request.
- **Token scope** [DOC] (`hub-docs/docs/xet/auth.md`):
  - The Hub token is presented as `Authorization: Bearer`.
  - Expected errors: 401 (bad Hub token), 403 (no write access), 404 (repo/revision missing).
  - A write token supersedes read scope.

### 1.3 Hangar contract for `GET /api/{type}s/{ns}/{name}/xet-write-token/{rev}[?create_pr=1]`

- **Auth.** Use the normal Hangar Hub auth (Bearer user token). Require write permission on the repo.
  - With `create_pr=1`, require only "may open PRs" permission.
  - `rev == "None"` means the default branch, or any PR ref when `create_pr=1`.
  - Return 404 if the repo is missing. Return 404 if the revision is missing, except `None`, and except when `create_pr=1`.
- **Response.** Status 200, `Content-Type: application/json`, body `{"accessToken","exp","casUrl"}`.
  - Also set `X-Xet-Cas-Url`, `X-Xet-Access-Token` and `X-Xet-Token-Expiration`. The Python header parser (`utils/_xet.py::parse_xet_connection_info_from_headers`) is only used on the download path, which we never trigger, but setting them costs nothing.
- **Token format.** Hangar is the only verifier, so it can be an HMAC-signed blob carrying `{repo_id, repo_type, scope: write, exp, user}`.
  - The docs allow up to 64000 characters for both `accessToken` and `casUrl`.
  - Pick a TTL such as 1 h. The client refreshes 30 s before expiry, so a short TTL is fine.
- **Choosing `casUrl`.** API calls use `format!("{endpoint}/v1/...")`, which keeps any path prefix. Telemetry is the exception:
  - It does `base.join("/v1/telemetry")` (`xet_client/src/cas_client/telemetry/mod.rs` around line 123), which drops the path. So `casUrl = https://hangar/cas` sends telemetry to `https://hangar/v1/telemetry`.
  - Either serve `POST /v1/telemetry` at the host root (return 200, discard the body), or give CAS its own hostname. The client never retries telemetry and ignores the response (`openapi/cas.openapi.yaml` `/v1/telemetry`), so a 404 there is harmless apart from log noise.
  - **Keep `casUrl` stable.** The client's local shard cache and resume-staging directory are keyed on it (§6).

### 1.4 `xet-read-token`: return 404

Nothing on the upload path calls `xet-read-token`. The read-token URL is only built on the download path:

- From `X-Xet-Hash` plus `Link: rel="xet-auth"` / `X-Xet-Refresh-Route` on resolve responses (`parse_xet_file_data_from_response`).
- From the tree cache, when a tree entry has `xet_hash`. See `file_download.py` around line 1915, which builds `xet_connection_info_refresh_url(token_type=READ, …)` from `entry.xet_hash`. Tree entries get `xetHash` from list-tree / paths-info JSON (`hf_api.py` line 699 `self.xet_hash = kwargs.pop("xetHash", None)`).

So Hangar must **never emit** any of these:

- `X-Xet-Hash`, `X-Xet-Refresh-Route`, or `Link: <…>; rel="xet-auth"` on resolve/HEAD responses.
- `X-Xet-Cas-Url`, `X-Xet-Access-Token`, `X-Xet-Token-Expiration` on non-token routes.
- `xetHash` in tree or paths-info entries.

With that in place, returning 404 from `xet-read-token` is safe.

Side effect: `HfApi` cross-repo copy of LFS files raises `ValueError` when the source file has no `xetHash` (`hf_api.py` around line 5480; it calls the `/lfs-files/duplicate` endpoint). Same-repo copies use `lfsFile` with only an `oid` (`_commit_api.py` around line 990) and are unaffected. Treat the cross-repo copy as a known gap.

---

## 2. CAS endpoints on the upload path, in call order

Common request headers on every CAS call [SRC, `common/http_client.rs::build_auth_http_client`]:

| Header | Value |
|---|---|
| `Authorization` | `Bearer <accessToken>` |
| `X-Xet-Session-Id` | UUIDv7 (`cas_types/mod.rs::SESSION_ID_HEADER`) |
| Custom headers | huggingface_hub's headers minus Authorization (user-agent and similar) |

Xorb and shard uploads also set `Content-Length` and stream the body. The client does **not** set `Content-Type`, so don't require it (the OpenAPI spec says `application/octet-stream`). A response may carry `X-Request-Id`, which the client logs.

Validate the token on every route:

- **Write scope required:** `POST /v1/xorbs/*`, `POST /v1/shards`, `POST /v2/shards`.
- **Read scope is enough:** `GET /v1/chunks/*`, telemetry.
- **Error codes:** 401 for an expired or invalid token. 403 for insufficient scope (the client does not refresh on either; see §1.2).

| # | Call | Notes |
|---|---|---|
| 0 | `GET {hub}/api/…/xet-write-token/{rev}` | Once at commit build, then again whenever `exp - 30s` passes. |
| 1 | `GET /v1/chunks/{prefix}/{hash}` (global dedup) | See 2.1. Runs in the background. |
| 2 | `POST /v1/xorbs/{prefix}/{hash}` | One per *new* xorb. Concurrent, adaptive. |
| 3 | `POST /v2/shards`, falling back to `POST /v1/shards` | After **all** xorbs in the session are done. Usually 1 shard; split if larger than 64 MiB. |
| 4 | `POST /v1/telemetry` at the casUrl host root | Fire-and-forget. |

Calls that are **not** made on the upload path [SRC, all endpoint calls in `xet_client/src/cas_client/remote_client.rs`]:

- `GET /v1|v2/reconstructions/*`, `POST /v1/reconstructions`: download only.
- `HEAD /v1/xorbs/*`, `HEAD /v1/files/*`: not called by the Rust client at all.
- `GET /v2/file-chunk-hashes/*`: used only by `xet_data/src/processing/range_upload.rs`. Neither the `hf_xet` Python binding nor `xet_pkg` exposes that path. A grep of `hf_xet/src` and `xet_pkg/src` for range-upload calls found nothing, and huggingface_hub only uses `start_upload_file` / `start_upload_bytes`.
- The "xorb upload grant" types (`cas_types/mod.rs::XorbUploadGrantRequest`) exist on `main` but nothing uses them. Watch future releases.

### 2.1 Global dedup query: return 404 every time

- **When it fires.** For the first chunk of each file, and for any chunk whose hash satisfies `u64_le(hash[24..32]) % 1024 == 0`. Sources: `metadata_shard/constants.rs::hash_is_global_dedup_eligible`, `merklehash/data_hash.rs` `impl Rem` (`self[3].to_le() % rhs`), `hub-docs/deduplication.md`. Enabled by default (`config/groups/deduplication.rs::global_dedup_query_enabled = true`).
- **Prefix mismatch.** The doc says `GET /v1/chunks/default-merkledb/{hash}`. The client actually sends `GET /v1/chunks/default/{hash}`:
  - `shard_interface/native.rs::query_dedup_shard_by_chunk` passes `ctx.config.data.default_prefix`.
  - That prefix is `"default"` (`config/groups/data.rs`).
  - `remote_client.rs::query_dedup_api` formats `/v1/chunks/{prefix}/{hash:x}`.
  - **Accept any prefix.**
- **Returning 404 is safe.** `query_dedup_api` runs with `.with_expected_404()`. It maps **any** HTTP-status error to `Ok(None)`, and the caller treats errors with `info_error` (not found). `query_dedup_shard_by_chunk` then returns `false` and the chunk is uploaded normally. [SRC]
- Return a plain `404`; it is not retried. Avoid 5xx, which would be retried with backoff first.
- The docs mark dedup as "an OPTIONAL component of the upload process" (`upload-protocol.md` step 2).

### 2.2 `POST /v1/xorbs/{prefix}/{hash}`

- **Path.** `prefix` is `default` (`data.rs::default_prefix`). Accept any value but key storage by hash only. `hash` is the 64-hex API string form (§3.5).
- **Body.** A serialized xorb **without footer** [SRC]:
  - `xet_data/src/processing/file_upload_session.rs` (around line 387) calls `SerializedXorbObject::from_xorb(xorb, false /*serialize_footer*/, …)`, with the comment "XORBs are sent without footer - the server/client reconstructs it from chunk data."
  - guilt/xet-server PROTOCOL.md §2 confirms this from captured traffic.
  - Still accept a body that ends in a footer, because other clients or older versions may send one. See §3.3.
- **Response.** 200 with `{"was_inserted": true|false}` (`cas_types/mod.rs::UploadXorbResponse`). `false` means it already existed, which is not an error.
- **Errors** [DOC, `api.md` §3]:
  - 400: malformed hash, hash mismatch, or bad serialization.
  - 401 / 403 as above.
  - The xorb doc says the server rejects xorbs above 64 MiB serialized. Use 400 or 413; the client treats both as non-retryable.
- **Limits:**
  - At most 64 MiB serialized (`xorb.md`; `xorb_object/constants.rs::MAX_XORB_BYTES`).
  - At most 8192 chunks (`MAX_XORB_CHUNKS`; `deduplication.md`).
  - Each chunk at most 128 KiB uncompressed (`MAX_CHUNK_SIZE = 64Ki*2`, enforced in `xorb_chunk_format.rs::XorbChunkHeader::validate`). Compressed length at most `2*MAX_CHUNK_SIZE`.
- **Idempotency.** The same hash may be POSTed again, for example by a different machine or a retry.
  - Return `was_inserted:false`.
  - **Do refresh the xorb's "last upload" timestamp.** The reference simulation stamps `last-upload` on every write, "mirroring" production CAS (`simulation/local_client.rs::upload_xorb`). Hangar's retention clock depends on this (§6).
  - Re-validating an existing hash is optional. If you skip it, never overwrite the stored copy with unvalidated bytes.

### 2.3 `POST /v2/shards`, then `POST /v1/shards`

- **Version selection.** The client tries v2 first. On **404 or 501** it falls back to v1 and caches the choice per `RemoteClient` (`remote_client.rs::upload_shard_with_version_override`) [SRC].
  - **v1 MVP:** answer `/v2/shards` with 404.
  - v1 shard uploads use a client with **no read timeout** (`build_auth_http_client_no_read_timeout`), because server validation can take a long time.
- **v1 response.** 200 with `{"result": 0|1}` (`UploadShardResponse`; 0 = exists, 1 = SyncPerformed).
  - An optional `"shard_hash": "<hex>"` is accepted from `main` onward. Older clients ignore unknown fields (serde default). Omit it.
  - Any 200 means success (`api.md` §4).
- **v2 response.** 200 `application/x-ndjson` with one event per line. Success or failure is decided by the terminal event, not the status code [DOC `api.md` §5; SRC `remote_client.rs::upload_shard_v2`, `local_server/handlers.rs::post_shard_v2`]:
  - `{"type":"validating","verified":n,"total":m}`
  - `{"type":"committing","stage":"uploading"|"syncing"}`
  - `{"type":"result","result":1}`, or `{"type":"error","message":"…","retryable":bool}` (the `retryable` field is in the simulation server and client parser).
  - Production re-emits the last frame about every 20 s as a heartbeat. The v2 client relies on that because it runs *with* a read timeout.
  - **Implement v2 if any proxy in front of CAS has an idle or response timeout.** Example: Cloudflare's 100 s origin-response limit (Bellatrix remote access goes through a Cloudflare Tunnel). Hangar does reassembly synchronously inside this call (§5), and that can take minutes for large files.
- **Body.** A shard without footer, `footer_size = 0` (`shard_interface/native.rs::read_shard_to_bytes_remove_footer`; `shard.md` "MUST be omitted"). Format in §4.
- **Errors.**
  - 400: bad serialization, failed verification, unknown xorb, or too large (at most 64 MiB per `upload-protocol.md` §6).
  - 401 / 403 as above.
  - **The client treats a 400 as fatal for the whole upload commit.** That matters for §6.

---

## 3. Xorb format and verification

### 3.1 Layout as uploaded

The body is a concatenation of `[8-byte chunk header][compressed payload]`, repeated, with no xorb header [SRC `xorb_chunk_format.rs`; DOC `xorb.md`].

| Offset | Size | Field | Rule |
|---|---|---|---|
| 0 | 1 | `version` | Must be 0 (`CURRENT_VERSION`); greater is rejected |
| 1 | 3 | `compressed_length` | u24 LE; at most 2·131072 |
| 4 | 1 | `compression_scheme` | 0 None, 1 LZ4, 2 ByteGrouping4LZ4. 99 (`Auto`) is never valid on the wire; anything else is an error |
| 5 | 3 | `uncompressed_length` | u24 LE; at most 131072 |

The payload is exactly `compressed_length` bytes. Decompressed output must be **exactly** `uncompressed_length` bytes, otherwise the xorb is corrupt (`deserialize_chunk_with_header_to_writer`).

The client stores a chunk as scheme 0 when compression doesn't shrink it (`serialize_chunk`). A single xorb can mix schemes.

### 3.2 Decompression in Go

- **LZ4 = LZ4 *frame* format.**
  - `xorb_object/compression_scheme.rs` uses `lz4_flex::frame::{FrameEncoder, FrameDecoder}` with default settings (`FrameEncoder::new(...)`, then `write_all`, then `finish`). [SRC]
  - guilt/xet-server decoded a captured frame from real traffic (`PROTOCOL.md` §4).
  - The doc's "Standard LZ4 compression" is ambiguous; the frame format is correct.
  - **Go:** `github.com/pierrec/lz4/v4`. `lz4.NewReader(io.LimitReader(payload, compressedLen))` decodes standard frames (magic `0x184D2204`) and handles any block size, block mode and checksum flags. wzshiming/xet uses exactly this (`xorb/compression.go` pools `lz4.Reader`/`lz4.Writer`).
  - [UNVERIFIED] I believe lz4_flex's default `FrameInfo` is 64 KB independent blocks with no checksums. That doesn't matter, because pierrec handles every variant.
- **Bomb guard.** Read from the lz4 reader through `io.LimitReader(r, uncompressedLen+1)`, and require exactly `uncompressedLen` bytes and a clean EOF. guilt/xet-server PROTOCOL.md §8 shows an unbounded decoder is a decompression-bomb vector.
- **BG4 (scheme 2)** [SRC `byte_grouping/bg4.rs`]:
  1. Run LZ4-frame decompression to get `g` of length `n`.
  2. Regroup. With `s = n/4` and `r = n%4`, the group lengths are `[s+(r>0), s+(r>1), s+(r>2), s]`, laid out consecutively in `g`.
  3. Output `out[4i+k] = group_k[i]` for `i < s`, then the remainder bytes `out[4s+k] = group_k[s]` for `k < r`.
- **None (scheme 0).** Copy the payload as-is.

### 3.3 Optional footer (accept and strip)

The footer format is `XorbObjectInfoV1` (`xorb_object_format.rs`): ident `"XETBLOB"`, version 1, xorb hash, then hash and boundary sections, then a 16-byte nonce buffer, then a trailing u32 `info_length`. hf_xet doesn't send it.

Detection, as the reference server does it (`xorb_chunk_format.rs::parse_chunk_header`): if the next 8 header bytes start with `XETBLOB`, chunk data has ended. `reconstruct_xorb_with_footer` stops there.

The reference simulation server always rebuilds its own footer from the chunks (`local_server/handlers.rs::post_xorb` with `footer_start: None`, then `local_client.rs::upload_xorb`, then `reconstruct_xorb_with_footer`).

**Hangar:** parse chunks until EOF or `XETBLOB`. Store only the chunk region. Keep a sidecar index per chunk: compressed offset, compressed length, uncompressed length, and chunk hash.

### 3.4 Hash functions (all BLAKE3 keyed, 32-byte keys)

Sources: `merklehash/data_hash.rs`, `merklehash/aggregated_hashes.rs`, `metadata_shard/chunk_verification.rs`, `hub-docs/hashing.md`. [SRC]

- **`DATA_KEY`** (chunk hash = `blake3.keyed(DATA_KEY, chunk_bytes)`):
  `[102,151,245,119,91,149,80,222,49,53,203,172,165,151,24,28,157,228,33,16,155,235,43,88,180,208,176,75,147,173,242,41]`
- **`INTERNAL_NODE_KEY`**:
  `[1,126,197,199,165,71,41,150,253,148,102,102,180,138,2,230,93,221,83,111,55,199,109,210,248,99,82,230,74,83,113,63]`
- **`VERIFICATION_KEY`**:
  `[127,24,87,214,206,86,237,102,18,127,249,19,231,165,195,243,164,205,38,213,181,219,73,230,65,36,152,127,40,251,148,195]`
- **File-hash key:** 32 zero bytes. `file_hash = blake3.keyed(zeros, xorb_style_root.raw_bytes)` (`file_hash_with_salt`, `DataHash::hmac`).

**Merkle root** (`aggregated_node_hash`, shared by xorb hash and file hash). Port it exactly:

```
nodes := [(chunkHash, chunkLen)...]           // empty -> zero hash
if len(nodes)==0 { return [32]byte{} }
for len(nodes) > 1 {
  out := nil; i := 0
  for i < len(nodes) {
    cut := nextMergeCut(nodes[i:]); out = append(out, merge(nodes[i:i+cut])); i += cut
  }
  nodes = out
}
return nodes[0].hash                           // single chunk => root == chunk hash (no internal node)

nextMergeCut(h): if len(h) <= 2 { return len(h) }
                 end := min(9, len(h))          // 2*BF+1, BF=4
                 for i := 2; i < end; i++ { if u64le(h[i].hash[24:32]) % 4 == 0 { return i+1 } }
                 return end
merge(group): buf = for each (h,s): apiHex(h) + " : " + decimal(s) + "\n"
              return (blake3.keyed(INTERNAL_NODE_KEY, buf), sum(s))
```

Two details are easy to get wrong:

- `apiHex(h)` is the **API string form** (§3.5), not plain hex of the 32 bytes. `write_hash_entry` writes `write_hex_u64(h[k].to_le())` for k = 0..3. The hashing doc's "lowercase hex format" is ambiguous on this.
- A group of 1 can occur at a level below the top, for example a 1-element tail. It still gets hashed as an internal node.

**Hashes:**

- **Xorb hash:** merkle root over the xorb's chunks.
- **File hash:** blake3 keyed with zeros over the merkle root of **all chunks of the file in order**, across all terms.
- **Term verification hash** (`range_hash_from_chunks`): `blake3.keyed(VERIFICATION_KEY, concat(raw 32-byte chunk hashes for xorb chunks [start,end)))`.

**Go libraries:**

- BLAKE3 keyed: `github.com/zeebo/blake3` (`NewKeyed`), used by wzshiming/xet and guilt/xet-server, or `lukechampine.com/blake3`.
- [UNVERIFIED] Both ship assembly for amd64 only, so arm64 (the Pis) runs pure Go. Measure throughput on the Pis.

**Test vectors.** HF dataset `xet-team/xet-spec-reference-files`:

- Three `.chunk` files.
- `Electric_Vehicle_Population_Data_20250917.csv` with `.chunks`, `.xet-file-hash` (`118a5332…af97`) and `.xet-xorb-hash` (`eea25d6e…7632`).
- Range hash `d81c11b1…4178` (796 chunks).
- A no-footer upload shard `.shard.verification-no-footer` and a dedupe shard.

These cover every hash path above.

### 3.5 Hash ↔ string

A 32-byte hash is four little-endian u64 values. The string form is `%016x` of each u64 in order, which means each 8-byte group is printed byte-reversed (`DataHash::hex`; `api.md` "Converting Hashes to Strings").

This form is used in URL paths, in the `.xet-*-hash` reference files, and inside merkle internal-node buffers.

Shards, xorb footers and the verification-hash input all use the **raw** bytes.

### 3.6 What the server must verify on xorb POST

This mirrors `XorbObject::validate_xorb_object` and the hash check in `reconstruct_xorb_with_footer`:

1. Size at most 64 MiB, chunk count at most 8192, and at least 1 chunk.
2. Every header is valid, every chunk decompresses to exactly its declared length, and no bytes trail the last chunk (other than an optional footer).
3. Compute the chunk hashes, then `xorb_hash(chunks)`, and check it **equals the hash in the path**. Otherwise return 400 (`api.md` §3: "Xorb hash does not match the body").

Verification is necessary, not optional. Without it, anyone could pre-POST garbage under a hash that honest users will later produce. The server would answer those users with `was_inserted:false` and keep the bad bytes, and their reassembly would then fail.

---

## 4. Shard format (upload)

Sources: `hub-docs/shard.md` [DOC], `metadata_shard/shard_format.rs`, `file_structs.rs`, `xorb_structs.rs`, `streaming_shard.rs` [SRC]. All integers are little-endian. Every record is 48 bytes.

### 4.1 Header (48 B)

| Field | Size | Value |
|---|---|---|
| Tag | 32 B | `MDB_SHARD_HEADER_TAG` = `"HFRepoMetaData\0"` (bytes `72,70,82,101,112,111,77,101,116,97,68,97,116,97,0`) followed by `85,105,103,69,106,123,129,87,131,165,189,217,92,205,209,74,169` (`shard_format.rs`) |
| `version` | u64 | 2 |
| `footer_size` | u64 | 0 on upload |

If `footer_size` is non-zero, parse the sections linearly and ignore the trailing footer.

### 4.2 File info section

A sequence of file blocks, ended by a bookend: 32×`0xFF` followed by 16×`0x00`.

| Record | Layout |
|---|---|
| `FileDataSequenceHeader` | `file_hash[32]`, `file_flags u32`, `num_entries u32`, `_unused[8]` |
| `FileDataSequenceEntry` × `num_entries` | `xorb_hash[32]`, `cas_flags u32` (0), `unpacked_segment_bytes u32`, `chunk_index_start u32`, `chunk_index_end u32` (exclusive) |
| If `flags & 0x80000000`: `FileVerificationEntry` × `num_entries` | `range_hash[32]`, `_unused[16]` |
| If `flags & 0x40000000`: `FileMetadataExt` × 1 | `sha256[32]` (raw digest bytes of the file content), `_unused[16]` |

- Real hf_xet clients set **both** flags (`file_flags = 0xC0000000`). Verification is always on (`file_deduplication.rs` `FileDataSequenceHeader::new(file_hash, n, true, metadata_ext.is_some())`). The guilt capture confirms this.
- **The metadata extension carries the SHA-256.** It is present whenever `Sha256Policy` is `Compute` or `Provided` (`file_cleaner.rs` around line 246: `metadata_ext = sha256.map(FileMetadataExt::new)`). huggingface_hub always passes one of these two: the sha256 hex when it already knows it, otherwise `hf_xet.COMPUTE_SHA256`.
- **A `Provided` sha256 is not checked by the client.** Hangar must recompute it (§5).

### 4.3 CAS info section

Describes only the xorbs **newly created** in this session, so a file may reference xorbs that are not listed here. Ended by a bookend.

| Record | Layout |
|---|---|
| `XorbChunkSequenceHeader` | `xorb_hash[32]`, `xorb_flags u32` (0), `num_entries u32` (chunk count), `num_bytes_in_xorb u32` (total uncompressed), `num_bytes_on_disk u32` |
| `XorbChunkSequenceEntry` × `num_entries` | `chunk_hash[32]`, `chunk_byte_range_start u32` (**cumulative uncompressed offset** of the chunk in the xorb; `raw_xorb_data.rs`), `unpacked_segment_bytes u32` (chunk length), `_unused[8]` |

`XorbChunkSequenceHeader::new` sets `num_bytes_on_disk: 0`. I didn't trace whether anything fills it in later [UNVERIFIED], so **don't validate it**.

### 4.4 Version rules

Header version 2 and footer version 1. Reject other versions (`shard.md` "Version Compatibility").

### 4.5 Server validation on shard POST

1. Size at most 64 MiB. Never preallocate from an entry count; bound everything by the remaining body size. xet-core added this in commit `af1a3ff9` "Cap untrusted shard parser memory allocations" (`streaming_shard.rs::account_bytes`, `checked_section_bytes`).
2. Header tag and version, bookends present, no trailing garbage.
3. For every file:
   - The verification flag must be set (`shard.md`: "Verification Entries MUST be set for shard uploads"; the shard is invalid if only some files have it).
   - The metadata ext must be present for model, dataset and space repos (`shard.md` FileMetadataExt: REQUIRED for Git-based repos).
4. For every term:
   - The xorb exists in Hangar's xorb store. Look it up **globally, not per repo** (see §5.2).
   - `start < end <= xorb.num_chunks`.
   - `unpacked_segment_bytes == Σ len(chunks[start:end])`.
   - `range_hash == blake3.keyed(VERIFICATION_KEY, concat(chunk_hashes[start:end]))`. This proves the uploader holds the chunk hashes, which in practice means the data ("ensure that the client uploading the file in the shard authoritatively has access to the actual file data", `upload-protocol.md` step 5).
5. `file_hash == blake3.keyed(zero, merkle(all (chunkHash, len) across terms))`. This is cheap from the sidecar indexes and needs no data read.
6. For every CAS-info block: the xorb exists, and its chunk count, chunk hashes, offsets and lengths match the stored index. A failure is a 400 (the doc says the shard may fail if "a referenced Xorb doesn't exist").
7. Then reassemble (§5). On success, return 200 `{"result":1}`.

### 4.6 Mapping the later commit's `lfsFile` to Xet

The Hub commit line is:

```json
{"key":"lfsFile","value":{"path":…, "algo":"sha256", "oid":<sha256 hex>, "size":<int>}}
```

It carries **no xet hash** (`_commit_api.py` around line 955).

So the mapping is entirely server-side: the shard's `FileMetadataExt.sha256` gives the sha256, and Hangar verifies it by recomputing during reassembly. Record `(xet_file_hash, sha256, size)` and the repo the shard was uploaded under. At commit time, `oid` is looked up in the whole-object store exactly as for classic LFS.

Ordering is guaranteed. huggingface_hub calls `xet_commit.wait_to_finish()`, which includes the shard upload, before it creates the Hub commit (`_upload_pipeline.py::_committer_loop` step 1; `_commit_api.py` uses the `with … as commit:` context manager). So doing reassembly synchronously inside the shard POST means the object exists by the time the commit arrives.

---

## 5. Reassembly and verification

### 5.1 Algorithm, per file block in an accepted shard

```
h := sha256.New(); total := 0; all := []chunkRef
tmp := createTemp()
for each term (xorbHash, start, end, unpackedBytes):
    idx := sidecarIndex(xorbHash)                 // per-chunk compressedOff/compressedLen/rawLen/hash
    r := openXorb(xorbHash) at idx[start].compressedOff
    for c in start..end-1:
        hdr := read 8B; payload := read hdr.compressedLen
        raw := decompress(hdr.scheme, payload, hdr.rawLen)      // §3.2, exact length
        // optional, defense in depth: blake3.keyed(DATA_KEY, raw) == idx[c].hash
        tmp.Write(raw); h.Write(raw); total += len(raw)
        all = append(all, (idx[c].hash, len(raw)))
    assert Σ rawLen(term) == unpackedBytes
assert fileHash(all) == hdr.file_hash          // can be done before reading data (§4.5)
assert h.Sum() == metadataExt.sha256           // reject with 400 if mismatched
fsync(tmp); rename(tmp, objects/sha256/<hex>)  // content-addressed, absent-or-identical
record xet_file(xet_hash, sha256, size, uploaded_by_repo, uploaded_at)
```

This matches `hub-docs/file-reconstruction.md`'s "Reconstruction Rules": concatenate the decoded chunk ranges in term order, with no gaps. Terms may reuse the same xorb or different xorbs, in any order.

- If the object already exists by sha256, skip the write. Still verify the hash relations, or trust an earlier verified record of the same `xet_file_hash → sha256`.
- An **empty file** gives `file_hash = 0` with zero terms (`xorb_hash`/`file_hash` return the default hash for an empty chunk list) [SRC]. Map it to sha256 `e3b0c442…b855`. [UNVERIFIED] whether huggingface_hub ever routes a 0-byte file to Xet; preupload probably makes it "regular".

### 5.2 Can segments reference xorbs from other files or repos?

**Yes.** A term may reference:

- Any xorb created earlier in the same session, including xorbs that sit in a different shard of the same session.
- Any xorb the client knows from its **local shard cache**. That cache is keyed by `casUrl`, **not by repo**:
  - `xet_data/src/processing/configurations.rs::compute_cache_path(cache_root, endpoint)`.
  - `shard_interface/native.rs::chunk_hash_dedup_query` queries the resumed-session manager, then the session manager, then the cache manager.
- Any xorb from a global-dedup response. Hangar never returns one.

So a model uploaded to repo A and then lightly modified and uploaded to repo B from the same machine produces a shard for B that points at A's xorbs. **Xorb lookup must be global.** The access-control boundary is the verification hash (possession of chunk hashes) plus the commit-time object check, not xorb ownership.

---

## 6. Deleting xorbs after reassembly

### 6.1 Clients will reference xorbs Hangar has deleted

What the source shows [SRC, `xet_data/src/processing/shard_interface/native.rs`]:

1. **Uploaded shards are cached locally for 3 weeks.** After every successful shard upload, the client copies its own shard into the cache directory with an expiry of `MDB_SHARD_LOCAL_CACHE_EXPIRATION = 21 days` (`upload_and_register_session_shards` → `export_with_expiration`; constant in `metadata_shard/constants.rs`). That shard's CAS-info section lists every xorb the session uploaded.
2. **Later sessions reuse those xorbs without uploading.** A later session with the same `casUrl` dedups chunks against that cache, and `chunk_hash_dedup_query` returns `true` meaning "already known to be uploaded". It then **does not upload the xorb**; it only references it in the new shard. The cache is used while `now <= shard_key_expiry` (`shard_file_handle.rs` around line 216) and deleted after expiry plus `MDB_SHARD_EXPIRATION_BUFFER` (7 days).
3. **Uploaded xorbs are also staged for resume.** Xorbs that uploaded successfully but whose commit never finished (crash, Ctrl-C, shard 400) are written to `staging/xorb_metadata` with the same 21-day expiry (`add_uploaded_xorb_block`). The next session merges them as a "resumed session" and also treats them as uploaded (`ShardSessionInterface::new` → `merge_shards_background`; `chunk_hash_dedup_query` returns `true` for resumed entries).
4. **A shard 400 is fatal.** The client has no "xorb missing, re-upload" recovery path: the 400 is non-retryable (`retry_wrapper.rs`) and nothing in `shard_interface` evicts cache entries on failure. Re-running the upload rebuilds the same shard from the same cache and fails the same way, until the cache expires or the user deletes `~/.cache/huggingface/xet/<endpoint-tag>/`.

Common, normal workflows trigger this. Re-uploading the same weights to a second repo is the obvious one, as is pushing a fine-tune that shares tensors with a base model uploaded from the same box.

**Conclusion.** Deleting xorbs right after reassembly breaks those uploads for up to 21 days, and the user's only fix is to clear their cache. Rejecting with 400 is the protocol-sanctioned response: `shard.md` "Shard Key Expiry" says references after expiry "can be rejected at the server's discretion". But inside the 21-day window, a 400 is a user-visible bug.

### 6.2 Options

| Option | Behaviour | Cost |
|---|---|---|
| **A. Keep xorbs for a retention window, then GC** (recommended for v1) | Keep a xorb until `last_seen + 28 days`. `last_seen` is the latest of: any POST of that xorb (including `was_inserted:false`), or any accepted shard listing it in CAS info. Shards that only *reference* a xorb in file terms don't extend the window, because clients don't re-cache those xorbs. 28 = 21-day client expiry + 7-day margin for client clock skew and long sessions. After GC, a shard referencing the xorb gets a 400 with a clear message ("xorb expired; clear ~/.cache/huggingface/xet"). | Temporary double storage: whole object plus xorbs for about 4 weeks after each upload. Xorbs are about the size of the data for incompressible weights; LZ4/BG4 savings vary. A GC job is needed. |
| B. "Virtual xorbs" | After reassembly, keep only the xorb's chunk index plus, per chunk, a pointer `(sha256 object, byte offset)` into a whole object that contains it. Delete xorb bytes as soon as every chunk is covered. Later shards referencing that xorb are rebuilt by reading the ranges out of whole objects. | No double storage. More complex: coverage tracking (xorbs from aborted commits have uncovered chunks, so keep bytes until window expiry), object-GC coupling (an index entry dies with its object), random reads across objects. A sensible v2. |
| C. Rotate `casUrl` (for example per week) | The client cache and resume staging are keyed on `casUrl`, so a new URL starts an empty cache and caps how far back clients can reference. | Hack. It scatters per-URL cache directories on client disks, breaks resume across rotations, and still needs retention of at least rotation period plus session length. Not recommended. |
| D. Delete immediately, reject with 400 | Fails the workflows above. | User-visible breakage. Reject. |

---

## 7. Licence and existing Go code

- **xet-core is Apache-2.0** (GitHub licence API; `xet-core/LICENSE`). Porting logic is fine with attribution and a NOTICE.
  - The hub-docs spec is Apache-2.0 too.
  - The reference **server** behaviour in xet-core is only the test simulation (`xet_client/src/cas_client/simulation/local_server/*`, `local_client.rs`). Production CAS (`cas_server`) is closed source; xet-core only mentions it in comments such as "see `validate_shard_from_async_read` in cas_server".
- **Go implementations.** All MIT, all young, all low-adoption:

| Repo | Created → last push | What it has | Notes |
|---|---|---|---|
| `wzshiming/xet` | 2026-03-24 → 2026-10-13 | Client and server: `server/` (`/v1/xorbs`, `/v1/shards`, `/v2/shards`, `/v1/chunks`, reconstructions), `xorb/` (encode/decode/validate/footer, pierrec/lz4, BG4), `shard/` (encode/decode/validate), `storage/` (local, S3, GC, `putfile`, `reconstructed_file.go`), and an `/xet-bridge/{sha256}` route. Deps: `zeebo/blake3`, `pierrec/lz4/v4`. | Most complete and most active, 6 stars. Its README notes that pre-1.1.10 hf_xet chunking differs, so old upstream xet hashes may not match. The SHA-256 is unaffected, so this doesn't matter to Hangar. |
| `jmwoliver/xet-go` | 2026-08-04 → 2026-08-20 | "CGO-free Go implementation of the Xet v1.1 data plane". Packages `xethash`, `chunker`, `xorb`, `shard`, `xetcas` with an optional `xetcas/server`, plus content stores (SHA-256 by length), catalogs, and a `conformance/` suite including `go_cas_rust_client_test.go` (Go CAS driven by the Rust client). | Cleanest separation for embedding. Uses a forked blake3 (`jmwoliver/blake3`). 1 star. |
| `guilt/xet-server` | 2026-09-11 → 2026-09-16 | Wire-compatible Hub + CAS server tested with the real `hf` CLI. Own LZ4 frame/block decoder, BG4, fuzz and adversarial tests. Dep: `zeebo/blake3` only. | 1 star. Its `docs/PROTOCOL.md` is a useful list of real-traffic quirks and independently confirms several findings here (no xorb or shard footer, LZ4 frame format, JSON token body, both file flags set, allocation caps, decompression bombs). [UNVERIFIED] Its §13 says the Xet upload starts from the LFS batch endpoint. That describes an older or git-xet flow and contradicts your v2.2.0 finding (preupload, then `new_upload_commit`); not checked. |

  Recommendation: **don't take any of these as a dependency.** All are weeks to months old, have a single maintainer and roughly zero adoption. **Do** borrow their parsers and tests (MIT, attribution), and run their test vectors against Hangar's port. The most relevant files to read are wzshiming's `xorb/` and `shard/validate.go`.
- **IETF draft.** wzshiming/xet's README cites "XET Protocol Draft" `draft-denis-xet-05` on the IETF datatracker. [UNVERIFIED] I didn't read it.

---

## 8. Effort, risk and minimum test plan

### 8.1 Components

Rough engineering estimate for one developer familiar with Go. These are judgement, not measurements.

| Component | Estimate |
|---|---|
| Token endpoint plus HMAC CAS token, `None` / `create_pr` / `%2F` handling | 0.5–1 day |
| Hash primitives (keyed BLAKE3, aggregated merkle, API-hex) with reference-file tests | 1–2 days |
| Xorb ingest: chunk parser, pierrec LZ4 frame + BG4 with bomb guards, footer tolerance, hash verification, sidecar index, atomic store, `last_seen` | 2–3 days |
| Shard parser and validator (flags, bookends, allocation caps, terms, verification hashes, file hash, CAS-info cross-check) | 2 days |
| Reassembly to sha256 object plus `xet_file` and repo records, commit-side lookup | 2–3 days |
| `/v2/shards` NDJSON with heartbeats (needed behind Cloudflare) | 1 day |
| Xorb retention GC (option A) | 1–2 days |
| E2E harness with real hf_xet, fuzzing of both parsers | 2–4 days |
| **Total** | **About 2.5–3.5 weeks** |

Option B (virtual xorbs) adds about another week.

### 8.2 Minimum test plan

1. **Unit, using vectors from `xet-team/xet-spec-reference-files`:**
   - Chunk hashes.
   - File hash `118a5332…` and xorb hash `eea25d6e…` from `.chunks`.
   - Range hash `d81c11b1…`.
   - Parse `eea25d6e….xorb` (decompress every chunk, recompute the xorb hash) and the `.shard.verification-no-footer` shard, then reassemble the CSV and compare its SHA-256 with the file.
2. **Format edge cases:**
   - Xorb with footer and without.
   - Mixed schemes in one xorb.
   - BG4 lengths with `n%4` in {0, 1, 2, 3}.
   - 1-chunk xorb and file (root equals chunk hash).
   - A merkle level that leaves a 1-element tail group.
   - Truncated, oversized and bomb chunks.
   - Shard with entry counts far larger than the body.
3. **E2E with real clients** in CI, pinned to hf_xet **1.6.0** and the latest 1.x, with `huggingface_hub==2.2.0` and `HF_ENDPOINT=http://hangar`. Assert the Hub commit's `lfsFile` oid equals `sha256sum` and the stored object is byte-identical.
   - `upload_file` of a small (< 8 KiB), a medium (about 5 MB) and a large (> 64 MiB, so several xorbs) file.
   - `upload_folder` with many files, enough to force several xorbs per file and several files per xorb.
   - `create_pr=True`.
   - The `HF_XET_SHARD_API_VERSION` path: default v2 returns 404 then v1. Also test v2 itself if implemented. [UNVERIFIED] the exact environment-variable name for `client.shard_api_version`.
   - Upload the same file to a **second repo**, and a modified copy, from the **same machine and cache**. Both must succeed, and Hangar's logs must show zero new xorbs for the second upload. This exercises §5.2 and §6.
   - Interrupt after the xorbs upload (kill during the shard step), then re-run. This exercises the resumed-session staging.
   - Token expiry: issue a 60 s TTL and upload a file that takes longer than 2 minutes. Refresh calls must appear and no 401s.
   - Run with `HF_HUB_DISABLE_TELEMETRY=1` and without it (telemetry 200/404 must not matter).
   - Network capture (as guilt did) to snapshot the request sequence for regression.
4. **Negative tests:**
   - Shard referencing an unknown xorb gets 400.
   - Wrong verification hash gets 400.
   - Wrong metadata sha256 gets 400.
   - A read-scope token on POST gets 403.
   - An expired token gets 401.

---

## Recommendation

**Implement the Xet write path in Hangar v1.** It is well specified, the reference source confirms every wire detail, and the server side is small:

- One token route and three CAS routes: `/v1/xorbs`, `/v1/shards` (plus `/v2/shards`), and `/v1/chunks` returning 404.
- A telemetry stub.

With huggingface_hub 2.2.0 and hf_xet installed, there is no alternative upload path for LFS-mode files.

Concretely:

1. Token route returns a JSON body (plus the `X-Xet-*` headers) with an HMAC token and truthful `exp`. Use a stable `casUrl`, ideally a dedicated host, or serve `/v1/telemetry` at the root.
2. `GET /v1/chunks/*/*` always returns 404 (accept any prefix). No global dedup.
3. On xorb POST, verify everything (§3.6), store the no-footer chunk region plus a sidecar chunk index, and update `last_seen`.
4. On shard POST, validate fully (§4.5), then synchronously reassemble each file, verify the SHA-256 against `FileMetadataExt`, and store the whole object by SHA-256 (§5.1). Implement `/v2/shards` with heartbeats if Cloudflare or another timeout-bound proxy sits in front. Otherwise 404 it and let clients fall back to v1.
5. Commit handling stays classic LFS: `lfsFile.oid` must name an existing whole object, and Hangar should also check that the object was uploaded under this repo's token or is already in the repo's history. That stops someone who knows a sha256 from claiming another repo's private file.
6. **Don't delete xorbs immediately.** Keep each xorb for 28 days after it was last POSTed or listed in an accepted shard's CAS info, then GC (option A). Reject later references with a 400 that tells the user how to clear their cache. Consider option B (virtual xorbs pointing into whole objects) once storage overhead matters.
7. Never emit Xet download metadata (`X-Xet-*`, `Link rel=xet-auth`, `xetHash`), and return 404 from `xet-read-token`. Downloads stay plain HTTP.

## Risks

- **Retention vs. client caches (highest).** Deleting xorbs inside the client's 21-day cache window makes ordinary re-uploads fail permanently until the cache expires (§6). Retention (option A) mitigates this, at the cost of temporary double storage on the cluster's USB disks. Bugs in `last_seen` bookkeeping show up as hard upload failures.
- **Upstream protocol drift.** The xet-core `main` branch already carries unused "xorb upload grant" types (presigned direct upload), a reported `shard_hash`, and an NDJSON v2 shard API. A future hf_xet could prefer a new endpoint, though recent changes kept v1 fallbacks (404/501 → v1). Pin the tested hf_xet range in Hangar docs, run E2E against new hf_xet releases, and keep the 404/501 fallbacks working.
- **Docs and source disagree.** The docs omit the xorb footer, say plain "LZ4", use the `default-merkledb` prefix, and describe merkle hex ambiguously. A spec-only implementation fails in exactly these places. Treat the source and reference files as authoritative.
- **Long synchronous shard POST.** Reassembling multi-GB files on Pi hardware can take minutes. v1 clients have no read timeout, but proxies do (Cloudflare 100 s). Needs `/v2/shards` heartbeats or LAN-only CAS access.
- **Cloudflare Access in front of CAS.** hf_xet sends only the Bearer CAS token plus huggingface_hub's non-auth headers. A Cloudflare Access policy on the CAS hostname would block it unless that hostname bypasses Access and relies on Hangar's own token. [UNVERIFIED] how the current Bellatrix Access setup would treat it.
- **CPU on Raspberry Pi.** The server computes BLAKE3 over every uploaded byte (xorb verification) plus SHA-256 over every reassembled byte, plus LZ4 decompression.
  - [UNVERIFIED] Go BLAKE3 libraries have SIMD only on amd64.
  - [UNVERIFIED] The Pi 4's Cortex-A72 lacks the ARMv8 SHA-2 extensions, while the Pi 5's A76 has them.
  - Measure before committing to throughput targets.
- **Untrusted parsers.** Entry counts, u24 lengths and LZ4 frames are attacker-controlled. Cap allocations by remaining body size, cap decompression at the declared length, and fuzz both parsers. xet-core and guilt/xet-server both shipped fixes for exactly these issues.
- **A client-provided SHA-256 is not trustworthy.** `Sha256Policy::Provided` passes huggingface_hub's own value through unchecked. Always recompute during reassembly.
- **Cross-repo copy gap.** Without `xetHash` in tree entries, huggingface_hub's cross-repo LFS copy (`/lfs-files/duplicate` path) raises `ValueError`. Accept this or implement the duplicate endpoint separately.
- **Small third-party Go ecosystem.** The Go implementations are weeks old with single maintainers. Borrowing from them is fine; depending on them is not.
