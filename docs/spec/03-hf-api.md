# 03. Hugging Face Hub API

Hangar implements the Hub HTTP API that real clients use. The wire contract, with sources, example payloads and a MUST, SHOULD and MAY tag for every endpoint, is [`docs/research/hf-hub-api-contract.md`](../research/hf-hub-api-contract.md), researched against `huggingface_hub` v2.2.0 (2026-10-08). **That file is normative for W4.** This page records Hangar's decisions on top of it.

## Scope for v1.0

Hangar implements **every MUST and SHOULD** in the contract's checklist (section 13), and these MAY items, because the feature list needs them:

- tags and branch deletion
- LFS `verify`
- `securityStatus` and `securityFileStatus` (complete when sent)
- `safetensors`, `gguf`, `usedStorage`, `downloads` and `likes` in Repo info
- the discussions and pull request API (REPO-6)
- device-code OAuth for interactive `hf auth login` (`/oauth/device`, `/oauth/token`), backed by Hangar Tokens
- single-segment ID redirects (to a configured default Namespace, if set)
- the collections API (BROWSE-5)
- dataset Parquet listing (`/api/datasets/{repo}/parquet`), Croissant and `converts` refs ([14](14-datasets.md))
- Xet uploads (see "Xet" below): the token endpoint and CAS upload endpoints, with reassembly into whole objects

Not implemented: Spaces, Inference, Jobs, buckets, kernels, Xet downloads, global Xet chunk deduplication.

## Rules that are easy to get wrong

These come from the contract and are repeated here because breaking them breaks real clients.

1. **Route on the escaped path.** Revisions arrive as `refs%2Fpr%2F1`. Split `r.URL.EscapedPath()` into segments, then unescape each one. chi's default routing decodes `%2F` too early; use `RawPath`-aware routing or a custom matcher for the Hub routes.
2. **Dates** in every JSON response: UTC, `YYYY-MM-DDTHH:MM:SS.sssZ`. Never `+00:00`.
3. **Booleans** in queries and forms: accept `true`, `false`, `True`, `False`, `1`, `0`. Accept both `expand=a&expand=b` and `expand[]=a`.
4. **Errors** always carry `X-Error-Code` (`RepoNotFound`, `RevisionNotFound`, `EntryNotFound`, `GatedRepo`) when one applies, `X-Error-Message`, JSON `{"error": "..."}`, and `X-Repo-Commit` on `EntryNotFound` from `resolve`. Missing or unreadable private Repos: 404 `RepoNotFound` (401 when no token was sent). Bad token: 401 with `X-Error-Message: Invalid credentials in Authorization header`.
5. **`resolve`**:
   - `HEAD` and `GET` return `X-Repo-Commit`, quoted `ETag` and `X-Linked-Etag` (SHA-256 for LFS files, git SHA-1 otherwise), `X-Linked-Size` and `Content-Length`.
   - Never send `Content-Encoding`.
   - Answer from metadata in under 10 seconds.
   - Ranges return a real 206 with `Content-Range: bytes a-b/total`, and 416 past the end.
6. **Redirects**:
   - S3 `redirect` mode: return a 302 to a presigned URL on a different origin, with all metadata headers on the 302 itself.
   - Never redirect to another path on Hangar's own origin unless the target repeats every header.
   - Use 307, not 302, for anything that is not a `GET` or `HEAD` (moved Repos).
7. **Never emit Xet download data**: no `X-Xet-Hash`, `X-Xet-Refresh-Route`, `Link: rel="xet-auth"` or `xetHash`. Downloads then use plain HTTP whether or not `hf_xet` is installed.
8. **Siblings** in Repo info are complete and recursive (diffusers plans downloads from them). Never include `transformersInfo` with keys the client does not know, `inferenceProviderMapping` or `evalResults`.
9. **Pagination**: `Link: <absolute URL on server.public_url>; rel="next"`.
10. **`POST /api/repos/create`**: accept `visibility` and legacy `private`; a 409 still returns `{"url": ...}`. Create the initial Commit on `main` with a `.gitattributes` that matches the Hub's default LFS rules.
11. **`POST /api/validate-yaml`** always returns JSON.
12. **Preupload** returns `shouldIgnore` for every file. Upload mode policy: `regular` for files under `hfapi.regular_upload_max` (default 10 MiB, matching the Hub) unless the path matches `.gitattributes` LFS rules; `lfs` otherwise.
13. **LFS batch**:
    - Upload `href`s are presigned or carry a one-time signed query token, because the client sends no `Authorization` header on them.
    - Multipart is signalled by `chunk_size` in the header plus numbered part URLs (`ceil(size/chunk_size)` of them), and every part PUT must return an `ETag`.
    - On the filesystem backend, Hangar's own part URLs carry the signed token.
14. **Downloads over 50 GB**: the client refuses plain HTTP downloads over 50 GB when it knows the size. Implement the contract's off-origin redirect without `X-Linked-Size` for files over 50 GB in S3 `redirect` mode, and document the limit for `proxy` mode and the filesystem backend.
15. **`datasets-server`**: `datasets` calls `datasets-server.<host>/info` with a 100-second timeout. That name must either not resolve, or point at Hangar, which serves the Dataset Viewer API there ([14](14-datasets.md)). The docs recommend pointing it at Hangar; the Helm chart's Ingress can add it (`ingress.datasetsServerHost: true`).

## Download counting

Count a download when the `X-HF-Download-Counter: 1` header is present and the file is the Repo's counted file ([02](02-architecture.md)).

## transformers safetensors auto-conversion

When a model has `.bin` weights and no safetensors weights, `transformers` (`safetensors_conversion.py`) lists the Repo's Commits and discussions, looking for an open pull request titled ``Adding `safetensors` variant of this model``. If it finds none, it posts the Repo ID, the private flag and any token passed explicitly to `https://safetensors-convert.hf.space`. Against Hangar, that would leak private Repo names, and possibly a Hangar token, to a third party.

Requirement (MUST): loading a pickle-only model from Hangar with `transformers` never contacts `safetensors-convert.hf.space`.

- W4 chooses the mechanism after reading the current `transformers` code paths (both the background-thread path and the synchronous path in `modeling_utils.py`). Proposed: when the caller's `User-Agent` starts with `transformers/` and the Revision has no safetensors weights, `GET .../commits/{rev}` returns a 403 error with a clear message. This stops the conversion thread before it reaches the discussions check.
- `hangar env` also exports `DISABLE_SAFETENSORS_CONVERSION=1`, and the docs explain why.
- `compat/hf` runs the load with outbound network blocked except to Hangar, and fails if any connection to `hf.space` is attempted.

## Xet

`hf_xet` is installed by default with `huggingface_hub` on x86_64 and arm64. When it is present, every LFS-mode upload goes through Xet, with no negotiation and no fallback. To make uploads work without client configuration, Hangar implements the **Xet write path** and turns each uploaded file back into a whole LFS object addressed by SHA-256. Xet reads are never offered, so downloads always use plain HTTP.

The contract, with sources (hub-docs and xet-core at `e922f742`, covering hf_xet 1.6.x and 1.7.x), binary formats, hash keys and test vectors, is [`docs/research/xet-upload-research.md`](../research/xet-upload-research.md). **It is normative for W11.** Where the hub-docs and xet-core disagree, xet-core wins; the research lists the five known differences.

### What Hangar implements

| Endpoint | Behaviour |
|---|---|
| `GET /api/{type}s/{ns}/{name}/xet-write-token/{rev}[?create_pr=1]` | Needs `write` on the Repo (or the right to open a pull request with `create_pr=1`). `rev` arrives percent-encoded; the literal `None` means the default branch. Returns JSON `{accessToken, exp, casUrl}` and the matching `X-Xet-Cas-Url`, `X-Xet-Access-Token` and `X-Xet-Token-Expiration` headers. The token is an HMAC-signed, short-lived (`xet.token_ttl`, default 1 hour) credential scoped to the user and Repo. `exp` must be accurate, because hf_xet refreshes 30 seconds before it and never on a 401. |
| `GET /api/{type}s/{ns}/{name}/xet-read-token/{rev}` | 404. Never called while Hangar emits no Xet download data. |
| `GET {casUrl}/v1/chunks/default/{hash}` | Always 404 (no global chunk deduplication in v1.0). The client then uploads the chunk. |
| `POST {casUrl}/v1/xorbs/default/{hash}` | Verify the xorb fully (layout, decompression, chunk hashes, xorb hash) before storing; reject a mismatch with 400. Store under `xet/xorbs/<hash>` in the Blob store. Respond `{"was_inserted": bool}`. Idempotent. A xorb Hangar already has is verified the same way and not stored again, so `was_inserted: false` reaches only a client that sent the xorb's bytes. |
| `POST {casUrl}/v2/shards` | Streamed response with heartbeats (the research doc, section 2.3), so reassembly survives proxies with idle timeouts such as Cloudflare's 100 seconds. |
| `POST {casUrl}/v1/shards` | Fallback; responds `{"result": 1}`. |
| `POST /v1/telemetry` at the root of the host | 200, body discarded. hf_xet drops any path prefix of `casUrl` for this one call. |

`casUrl` is `<public_url>/xet-cas` by default, or `xet.cas_url` if an admin gives CAS its own hostname.

### Shard handling and reassembly

On a shard POST, Hangar validates the shard (research section 4.5). Then, for each file block, it:

1. checks every referenced xorb exists (looked up globally, not per Repo)
2. reassembles the file's bytes from the xorb chunk ranges, streaming
3. recomputes the SHA-256 and the Xet file hash while it streams, and checks both against the shard (the SHA-256 in `FileMetadataExt` comes from the client, so it is never trusted without recomputing)
4. stores the result as a normal LFS object (`lfs/sha256/...`), verified, and records the mapping in `xet_files` and the token's User as an uploader of the object ([04](04-git.md) "Who may reference an LFS object"). When the object is already stored, the rebuilt bytes are compared with it and not stored again. An error about a file never gives the size of a stored object.

Only then does the shard POST succeed. The commit that follows names files by SHA-256 in `lfsFile` lines, so it uses the normal commit path with no Xet awareness.

If a shard references a xorb Hangar does not have, it returns 400 with a message telling the user to clear the hf_xet cache (`~/.cache/huggingface/xet`) or set `HF_HUB_DISABLE_XET=1`.

### Keeping xorbs

hf_xet caches the shards it uploaded for 21 days, and later uploads from the same machine (to any Repo on the same CAS URL) reference those xorbs without sending them again. So Hangar keeps xorbs after reassembly:

- `xet_xorbs(hash, size, storage_key, last_referenced_at)` is updated on every xorb upload and every shard that references the xorb.
- GC deletes a xorb once `last_referenced_at` is older than `xet.xorb_retention` (default 28 days, longer than the client's 21-day cache).
- Storage cost: recently uploaded data is stored twice (once as whole objects, once as xorbs) for up to 28 days. This appears on the storage page as **Xet upload cache**, and admins can lower the retention (at the risk of the 400 above).

### Modes

`hfapi.xet` config:

- `ingest` (default): everything above.
- `reject`: the token endpoints return 400 with `X-Error-Message: Xet uploads are turned off on this Hangar. Set HF_HUB_DISABLE_XET=1 to upload.`, and preupload answers `regular` for files up to `hfapi.regular_upload_max`, so small uploads still work with no client setup.

### Not supported

- Xet downloads, and so the 50 GB plain HTTP download limit for single files still applies (rule 14).
- huggingface_hub's cross-Repo LFS copy needs `xetHash` in tree entries and raises an error against Hangar. Document it; copy within the same Repo works.
