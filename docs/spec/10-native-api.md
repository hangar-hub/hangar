# 10. Native API

The native API serves the web UI, the CLI and automation. It lives under `/api/hangar/v1`, is described by `api/openapi.yaml` (OpenAPI 3.1), and is the contract between backend and UI workstreams. Phase 0 of the build plan writes the full OpenAPI file from this outline; handlers implement the generated interfaces.

## Conventions

- JSON in and out; `snake_case` field names; RFC 3339 UTC times; sizes in bytes as integers.
- Errors: RFC 9457 problem details (`type`, `title`, `status`, `detail`, plus `errors[]` for validation).
- Lists: cursor pagination (`?cursor=&limit=`, default 50, maximum 500), response `{ "items": [...], "next_cursor": "..." }`.
- Auth: session cookie with CSRF header (`X-CSRF-Token`), or `Authorization: Bearer <token>`.
- Repo paths: `/repos/{type}/{ns}/{name}` where `type` is `models` or `datasets`.
- Revisions in paths are URL-encoded (`refs%2Fpr%2F1`).
- Every mutating call writes an audit event where [05](05-auth.md) lists one.

## Resources

| Area | Endpoints |
|---|---|
| Session | `GET /me`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/providers`, `GET/DELETE /me/sessions[/{id}]` |
| Tokens | `GET/POST /me/tokens`, `DELETE /me/tokens/{id}` |
| Keys | `GET/POST /me/ollama-keys`, `DELETE /me/ollama-keys/{id}`; `GET/PUT/DELETE /me/upstream-tokens/{upstream}` |
| Search | `GET /search?q=&types=repos,namespaces,collections` |
| Arsenal | `GET /repos?type=&q=&ns=&format=&task=&library=&quant=&params_min=&params_max=&context_min=&size_max=&licence=&mirror=&sort=&cursor=` (facet counts in `facets`) |
| Repos | `POST /repos`, `GET/PATCH/DELETE /repos/{type}/{ns}/{name}`, `POST .../restore`, `POST .../rename`, `POST .../transfer`, `GET .../refs`, `GET .../commits?rev=`, `GET .../commits/{sha}` (with diff summary), `GET .../commits/{sha}/diff?path=`, `GET .../tree/{rev}/{path}` (with presence, scan and last Commit), `GET .../metadata?rev=`, `GET .../files/{rev}/{path}/header` (safetensors or GGUF header JSON), `GET .../quants?rev=`, `GET .../dependencies` (needs and needed by), `POST/DELETE .../like`, `GET .../stats` (downloads by day), `GET/PUT .../collaborators`, `GET/POST .../access-requests`, `PATCH .../access-requests/{id}` |
| Uploads | `POST .../uploads` (body: files with path, size, sha256; response per file: `inline`, `exists`, or `upload` with an upload ID and part URLs), `GET .../uploads/{id}` (parts done), `PUT .../uploads/{id}/parts/{n}` (when not presigned), `POST .../uploads/{id}/complete`, `POST .../commits` (body: operations, message, branch, `create_pr`, `parent_commit`) |
| Zip | `POST .../zip?rev=` (starts a Job), then `GET /jobs/{id}` gives the download URL |
| Mirrors | `GET/PATCH .../mirror` (selection, tracking, schedule), `POST .../mirror/sync`, `POST .../mirror/detach`, `POST .../mirror/pull` (more files) |
| Discover | `GET /upstreams` (enabled ones, for users), `GET /discover/models?upstream=&search=&author=&filter=&sort=&cursor=`, `GET /discover/models/{ns}/{name}?upstream=&rev=` (upstream detail with files, quants, Dependencies preview, arsenal state), `POST /pulls` (upstream, repo, type, revision, selection), `GET /pulls/{id}` |
| Jobs | `GET /jobs?mine=&kind=&status=`, `GET /jobs/{id}` (progress, per-file detail for Pulls), `POST /jobs/{id}/cancel`, `POST /jobs/{id}/retry` |
| Namespaces | `GET /namespaces/{ns}`, `POST /orgs`, `PATCH/DELETE /orgs/{ns}`, `GET/PUT/DELETE /orgs/{ns}/members[/{user}]` |
| Collections | `GET/POST /collections`, `GET/PATCH/DELETE /collections/{ns}/{slug}`, `POST/PATCH/DELETE /collections/{ns}/{slug}/items[/{id}]` |
| Community | `GET/POST .../discussions`, `GET/PATCH .../discussions/{num}`, `POST .../discussions/{num}/comments`, `POST .../discussions/{num}/merge` |
| Webhooks | `GET/POST /webhooks`, `GET/PATCH/DELETE /webhooks/{id}`, `GET /webhooks/{id}/deliveries`, `POST /webhooks/{id}/deliveries/{id}/redeliver`, `POST /webhooks/{id}/test` |
| Admin | `GET/POST /admin/users`, `PATCH /admin/users/{id}`, `POST /admin/users/{id}/reset-link`, `POST /admin/invites`, `GET/POST/PATCH/DELETE /admin/upstreams[/{id}]`, `POST /admin/upstreams/{id}/test`, `GET/PATCH /admin/settings`, `GET /admin/storage` (usage by Namespace), `PUT /admin/quotas/{ns}`, `POST /admin/gc`, `GET /admin/audit?actor=&action=&since=&until=&format=csv`, `GET /admin/system` (version, backends with the S3 download and upload modes, telemetry, doctor checks), `GET /admin/tokens`, `DELETE /admin/tokens/{id}` |

## Webhook events

Payload: `{ "event": "...", "id": "<uuid>", "occurred_at": "...", "actor": {...}, "repo": {...}, "data": {...} }`.

Headers: `X-Hangar-Event`, `X-Hangar-Delivery`, `X-Hangar-Signature: sha256=<hex HMAC of body>`.

Events: `repo.created`, `repo.updated` (push or commit, with refs changed), `repo.deleted`, `repo.restored`, `repo.renamed`, `repo.visibility_changed`, `pull.started`, `pull.completed`, `pull.failed`, `mirror.synced`, `index.completed`, `scan.completed` (with status), `discussion.created`, `discussion.commented`, `discussion.status_changed`, `pr.merged`.

Delivery: timeout 10 seconds, retries at 1 minute, 5 minutes, 30 minutes, 2 hours and 12 hours, then marked failed. Webhook URLs to private address ranges are allowed only if the Instance setting `webhooks.allow_private_targets` is on (default on for self-hosting; document turning it off for multi-tenant Instances). Reserved addresses (link-local, which includes the cloud metadata service, other metadata services, unspecified, broadcast and multicast) are refused whatever the setting, as for Upstreams ([06](06-upstreams-and-pulls.md), "Network policy"). Both checks apply to the address each delivery connects to, after DNS resolution, and redirects are not followed.
