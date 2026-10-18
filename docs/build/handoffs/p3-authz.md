# Phase 3 handoff: authentication, authorisation, CSRF and browser security

Phase 3 security review of [`15-build-plan.md`](../../spec/15-build-plan.md), area "authz", on branch `ws/p3-authz` from `382cc88`. It covers `internal/auth`, `internal/httpx`, `internal/api`, the Hub API routing in `internal/hfapi`, and the web UI headers in `internal/web`. The threat model is written from this file.

## What was reviewed

- Every route of every router: the native API (chi), the Hub API, git smart HTTP, git-lfs, Xet CAS, the Ollama registry, the Dataset Viewer, the sign-in routes, the signed-link routes and the web UI, against the permission table in [05](../../spec/05-auth.md). `TestRouteAuthorisationMatrix` now enforces this (finding 6).
- Authentication: Bearer Tokens, Tokens as HTTP Basic passwords, the session cookie and its CSRF header, Ollama registry JWTs, Xet CAS tokens and signed URLs, and how each behaves in a browser.
- Token scopes against Roles, including the write-scope widening of commit `54e5307`, and what a Token can do with `/me/tokens`.
- Signed URLs (`httpx.Signer`) and presigned Blob store URLs: what each is scoped to and how long it lives.
- Secrets in logs, spans, metrics and OTLP log records, with a full server at debug level exporting to a local OTLP collector.
- Browser security headers on UI pages and on every route that serves file bytes.
- Ownership of LFS and Xet objects at Commit time, deferred from phase 2.

## Findings

| # | Severity | Finding | Fix | Test |
|---|---|---|---|---|
| 1 | High | Commit `54e5307` let a write Token do every Repo admin action (settings, rename, transfer, delete, webhooks, collaborators, access requests), through every API. James decided on 2026-10-17 ("Hub actions only") to allow only what huggingface_hub does on the Hub. | `425a744`: a new `hub_admin` action (write scope, admin Role) used only by `DELETE /api/repos/delete`, `PUT /api/{type}s/{repo}/settings` and a same-Namespace `POST /api/repos/move`; `admin` needs the admin scope again. A move checks the Namespace the Repo is in now, so an old ID that redirects to another Namespace cannot turn a transfer into a rename. Recorded in [decisions](../decisions.md) and 05. | `TestWriteTokenAdministersReposThroughHubAPIOnly` (full server, real Authorizer: each allowed Hub call, a transfer refused, the redirect case, a non-admin's write Token refused, and native delete, settings, rename, transfer, collaborators, access requests and webhooks refused with a write Token and allowed with an admin Token); `TestPermissionMatrix` (new `hub_admin` column); compat/hf `test_write_token_administers_own_repos` |
| 2 | High | CSRF through ambient credentials (phase 3 input 5). The Basic path had no CSRF check, and git and LFS answer 401 with a Basic challenge, so a browser that once opened a git URL caches the credentials and replays them on cross-site requests. | `5ad144f`: a state-changing request that carries Basic credentials or a session cookie is refused with 403 when `Sec-Fetch-Site` is present and is not `same-origin` or `none`, or, without it, when `Origin` names neither the request's host nor `public_url` (`net/http.CrossOriginProtection` with `public_url` trusted). Sign-in calls get the same check without a cookie (login CSRF). A Token as a Basic password is accepted only on git and git-lfs routes, as 05 already said; other API routes answer 401 and the static UI and health routes ignore it. | `TestCrossOriginBrowserCredentials` (8 browser header shapes against Basic on git and LFS, the cookie with a valid CSRF header, Bearer and sign-in), `TestBasicCredentialsOnlyForGit`, `TestWriteAuthError`; git push, LFS and Hub flows in `cmd/hangar` and the compat suites |
| 3 | Medium | A Token could create a Token that outlives it: `POST /me/tokens` checked the scope and restrictions of the calling Token but not its expiry, so an hour-long CI Token could mint a permanent one. | `5199150`: an expiring Token may only create Tokens that expire no later than it does. `TokenInfo` carries the expiry. | `TestSessionsTokensAndKeys` (no expiry and a later expiry refused, the same and an earlier expiry allowed) |
| 4 | Low | The signed form of a URL was ambiguous: `key=value` lines were not escaped, so a signed value holding a newline could be split into other parameters, for example a later `exp` that `Verify` reads first. No signed URL carries caller text in its query today, so this was not reachable. | `c1d1038`: keys and values are query-escaped before signing. Signed URLs issued before the upgrade stop verifying, which only means a client asks for a new one. | `TestSigner` (the forged later expiry; fails on the old form) |
| 5 | Low | Raw paths reached the route template: an unknown Dataset Viewer endpoint recorded `/datasets-server/<raw path>` as its route, which went into access logs, span names and metric attributes (unbounded cardinality, and caller text in telemetry). The signed-link routes (zip exports, Pull-through) recorded no template. | `4b8215c`: unknown Viewer endpoints record `/datasets-server/{endpoint}`; signed-link routes record `/api/hangar/v1/exports/{file}` and `/api/hangar/v1/pull-through/{repo}/{oid}`. | `TestRouteAuthorisationMatrix` (each probe must reach its route template) |
| 6 | Process | Nothing stopped a new route from shipping without an authorisation decision. | `4b8215c`: `TestRouteAuthorisationMatrix` walks the native router with `chi.Walk`, finds every other route declaration by parsing the routing packages (`httpx.SetRoute`, the Hub, Xet and auth route helpers, `ServeMux` registrations, the Viewer's endpoint table and the signed-link routes), and fails on any route without a `routeMatrix` entry or any entry without a route. It then probes all 222 routes on the full server as six callers and checks each rule. | `TestRouteAuthorisationMatrix` |
| 7 | Low | UI pages lacked `Cross-Origin-Opener-Policy` and `Permissions-Policy`. The CSP was already strict (no inline scripts, nonce styles, `frame-ancestors 'none'`). | `4b609f3`: both headers added; spec 09 lists the full header set. | `TestServesIndexForUIRoutesWithCSP` |

No secret was found in logs or telemetry. `06ee6fb` adds `TestSecretsStayOutOfLogsAndTelemetry`, which keeps it that way (see "Secrets in logs and spans" below).

## What the route matrix found to be correct

Each of these was checked by a probe, so the threat model can rely on it:

- Every Repo route refuses outsiders with 404 on a private Repo, and refuses anonymous callers with 401 or 404.
- Write routes refuse a read Token; admin routes refuse a write Token; `hub_admin` routes accept it.
- Every `/admin/*` route needs an Instance admin with an admin Token.
- `/auth/oidc/{provider}/link` and `/me/device` refuse every Token: linking a provider and approving a device login need a UI session.
- Signed-link routes (LFS object GETs and PUTs, LFS verify and multipart completion, exports, Pull-through, Viewer assets, Xet CAS, `/v2/token`) refuse a Hangar Token without their own signature or protocol token.
- `/api/hangar/v1/me` answers 204 to anonymous callers (bellatrix finding 6), by design. The Organisation member routes were public (residual risk 1); since 2026-10-18 they need a signed-in caller while `anonymous_read` is off.

## Secrets in logs and spans

- Reviewed: the access log (route template, never the raw path or query), `otelhttp` server spans (`url.path` only, never the query), the Upstream client spans (`url.template`), the URL redaction in `internal/upstream`, `internal/jobs`, `internal/cli` and `internal/webhook`, and the audit log's refusal of secret-looking keys.
- `TestSecretsStayOutOfLogsAndTelemetry` runs the full server with `log_level: debug` and traces, metrics and logs exported over OTLP/HTTP to a local collector that keeps the raw request bodies (OTLP protobuf stores strings verbatim). It signs in with a password, uses the session cookie and CSRF token, creates a Token, uses it as Bearer and Basic, stores an Upstream's Instance token, a user's upstream token and a webhook secret, uploads and downloads an LFS object through signed links, sends a wrong Token, a wrong Basic password, a stale cookie, an unknown device code and secrets in query strings, and makes a refused cross-origin change. None of the 16 secrets, and no `Bearer hgr_` or `Basic ` header, appears on stdout or in the exported bytes.

## Signed and presigned URLs

| URL | Scope | Lifetime |
|---|---|---|
| Hangar-signed LFS download (`lfs-download:<repo UUID>:<oid>`) | One object in one Repo | `storage.s3.presign_ttl`, 1 hour by default |
| Hangar-signed LFS upload, verify and multipart part URLs | One object or upload in one Repo | `lfs.upload_url_ttl`, 24 hours by default (spec 04) |
| Presigned S3 GET (LFS downloads, `resolve` redirects) | The object key, which is the SHA-256: any Repo holding the object | `storage.s3.presign_ttl`, 1 hour by default, 1 minute to 7 days |
| Presigned S3 PUT (LFS uploads) | A staging key for one object, verified before it is moved into place | `lfs.upload_url_ttl` |
| Zip export | One export object (`export:<key>`), and the unsafe-download gate runs again when it is used | 24 hours (`repo.ExportTTL`) |
| Pull-through | One object of one Mirror | 1 hour |
| Ollama blob | One digest of one Repo | 1 hour |
| Dataset Viewer asset | One asset path | 1 hour |

Signatures are HMAC-SHA256 with a key derived from the Instance key by HKDF, compared in constant time, and cover the path, scope, expiry and every other query parameter.

## Residual risks

1. **Namespace profiles and Organisation members are public even when `anonymous_read` is off.** `GET /api/hangar/v1/namespaces/{ns}` and `GET /orgs/{ns}/members[/{user}]` check `ActionRead` on the Namespace, which always passes, so an anonymous caller on a closed Instance can list usernames and Roles. Spec 05 defines `anonymous_read` for Repos only, so this needs a decision from James. Options: (a) keep them public, as on the Hub, and say so in 05; (b) when `anonymous_read` is off, require sign-in for Namespace profiles, member lists and `/search`, which changes no client flow (huggingface_hub never calls them anonymously). Decided by James on 2026-10-18: (b), with public Collections and a user's likes too (branch `fix/p3-6-misc`, recorded in [decisions](../decisions.md)).
2. **Ownership of LFS and Xet objects at Commit time (deferred from phase 2): documented, not implemented.** Spec 04 makes LFS objects Instance-wide and deduplicated ("objects that already exist and are verified get no `actions`"), and spec 03 looks Xet xorbs up globally, not per Repo. The contract is silent on ownership. So a user who knows the SHA-256 and size of an object stored anywhere on the Instance, including in a private Repo they cannot read, can commit a pointer to it in their own Repo and then download it; and the batch upload API is an existence and size oracle ("The size does not match: the stored object has N bytes"). The spec does not require a check, so none was added. The risk is bounded by needing the exact hash, which usually means having seen the file or its pointer (for example as a former collaborator, or from public weights that are also stored privately). Options for James: (a) accept and document; (b) deduplicate only when the object is referenced by a Repo the caller can read, or was uploaded by the caller, and otherwise ask for the bytes (`repo_lfs` gives the Repo side; uploads would need an uploader record, and `xet_files` a Repo column); (c) keep global deduplication for storage but check at Commit time that every new LFS pointer was either uploaded in this session by the caller or is readable by them, and drop the stored size from the mismatch message.
3. **Presigned S3 URLs are scoped to the object, not the Repo, and cannot be revoked.** A URL handed out before a user lost access keeps working until it expires (1 hour by default, up to 7 days if an admin raises `storage.s3.presign_ttl`). Hangar-signed URLs behave the same way for their own lifetimes. This is inherent to presigning; keep the default TTL short.
4. **Same-site requests are refused, cross-subdomain UIs are unsupported.** The origin check refuses `Sec-Fetch-Site: same-site`. Hangar serves its UI and APIs from one origin, so nothing breaks, but a deployment that puts a browser client on a sibling subdomain would need `public_url` to be that origin.
5. **Behaviour change for scripts that sent a Token with HTTP Basic to the Hub or native API.** They now get 401 with a message that says to use `Authorization: Bearer`. Spec 05 always listed Basic for git and git-lfs only, and no client in the compat suites, the CLI, huggingface_hub or Ollama sends it elsewhere.
6. **A write Token can force-push the default branch for a Namespace admin** (`gitserver.canForcePush` raises the scope for that one check). This predates the phase 3 decision and follows spec 04 ("force-pushing it is allowed only for Namespace admins"), which speaks of the Role, not the scope; git credentials are almost always write Tokens. It is listed so the threat model records it; James's decision did not name force pushes.
7. **Ollama blob responses carry `Content-Type: application/octet-stream` without `X-Content-Type-Options: nosniff`.** Browsers do not render `application/octet-stream`, so this is defence in depth only; `internal/ollama` belongs to another area and was left unchanged. Every other route that serves file bytes sends `nosniff`, an attachment disposition or a sandbox CSP.
8. **No HSTS header.** Hangar sets `Secure` on cookies when `public_url` is HTTPS and leaves HSTS to the proxy that terminates TLS (spec 09).
9. **The Dataset Viewer validates `where` before it checks access.** An anonymous caller gets 422 for a malformed filter on a private dataset; the answer does not depend on the dataset, so it reveals nothing.

## Not done, and why

- Phase 3 inputs 1, 2, 3, 4 and 6 (the unsafe-download gate, scan bypasses, Job starvation and Xet CPU amplification) belong to other phase 3 areas and were not touched here.
- The `nosniff` header on Ollama blobs (residual risk 7) is in `internal/ollama`, another area.
- `TestScanLimitsMakeResultsPartial` (`internal/datasets/viewer`) failed once in `make test` on a host with a load average near 50, with `partial: true` and 0 rows. It passed three times alone and in a full `-race` run of the package. The cause is in `scan.go`: the scanner checks `MaxScanTime` when `scanned % 4096 == 0`, which is already true before the first row, so a slow start ends the scan with no rows. It is a timing flake in another area, not caused by this branch, and is reported here for the Viewer owner.

## Audit round 3: probe race in the route matrix

The round 2 audit saw `TestRouteAuthorisationMatrix` fail once in about 41 runs on a loaded host. The probe read the route template for its request as soon as it had read the response, but `httpx.AccessLog` records the route only after the handler returns. A response the handler flushes (git `info/refs`, file streams) or one larger than the server's write buffer reaches the client first, so under load the probe could see no route and report `reached route ""`.

The probe now closes the response body and then waits, up to 30 seconds, for the access log line of its request ID (`routeLog` in `cmd/hangar/routes_authz_test.go`). Closing first matters: a handler still writing a large body returns only once the client stops reading. The server code is unchanged; the race was in the test. `TestProbeWaitsForLateAccessLog` reproduces it with a handler that flushes 8 KiB and logs only after the client hangs up: with the old probe it fails every time with an empty route, and it passes with the fix. `TestRouteLogWait` covers the wait itself.

## Tests run

All in `/Users/jamesgarside/Github/jamesagarside/hangar-wt/p3-authz` at the branch head.

| Run | Result |
|---|---|
| `make lint` (golangci-lint, compat vet, Biome, tsc, house style, spelling, test timeouts, test targets, Hub matrix) | Passed |
| `make test` with `HANGAR_TEST_POSTGRES=1 HANGAR_TEST_MINIO=1` (`go test -race -tags integration ./...` and vitest) | Every Go package passed except the Viewer timing flake above, which passed on a rerun (`ok internal/datasets/viewer 154.6s` with `-race`). UI: 25 files, 107 tests passed. `cmd/hangar` 994 s, `internal/auth` 149 s, `internal/api` 352 s, `internal/hfapi` 176 s, `internal/web` 2.6 s |
| `go test -tags compat ./compat/harness ./compat/git ./compat/hf ./compat/ollama` | All ok (git 166 s, ollama 46 s) |
| `uv run --project compat pytest compat/hf` | 42 passed, 2 skipped (the diffusers and `upload_large_folder` matrix cells), including `test_write_token_administers_own_repos`, `test_update_repo_settings`, `test_move_repo_redirects` and `test_delete_repo` |
| `TestRouteAuthorisationMatrix` | 222 routes probed as 6 callers, 78 s |
| `TestSecretsStayOutOfLogsAndTelemetry` | 16 secrets checked against 8 KB of debug logs and 269 KB of OTLP |
