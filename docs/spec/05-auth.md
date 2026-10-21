# 05. Users, access and authentication

## Identities

- A **User** has a personal Namespace, an email, an optional password, and zero or more OIDC identities.
- An **Organisation** is a Namespace with Members. Roles: `read` (see private Repos), `write` (push, create Repos), `admin` (manage Members, settings, delete).
- **Collaborators** get `read` or `write` on a single Repo.
- **Instance admins** can do anything, and are the only users who can manage Upstreams, Instance settings, quotas and the audit log.

## Permission rules

`internal/auth` exposes one function every handler uses:

```go
func (a *Authz) Can(ctx context.Context, p Principal, action Action, target Target) error // nil or ErrForbidden / ErrNotFound
```

| Action | Public Repo | Internal Repo | Private Repo |
|---|---|---|---|
| Read files, metadata, history | anyone if `anonymous_read`, else signed in | signed in | Namespace members (any Role), collaborators, Instance admins |
| Pull more files into a Mirror | `write` on the Namespace | same | same |
| Push, commit, branch, tag | `write` | `write` | `write` |
| Open discussion or PR, comment, like | signed in | signed in | readers |
| Settings, rename, transfer, delete, webhooks | Namespace `admin` | same | same |

- Users without read access to a private Repo get **404**, not 403, everywhere (Hub API, git, UI, Ollama), so private Repo names do not leak.
- Gated Repos: metadata is readable, files need an approved access request.
- Token restrictions narrow these rules and never widen them.
- **Token scope for Repo administration:** the last row needs the `admin` scope, with one exception that matches the Hub, whose Tokens are only read or write. A `write` Token may delete a Repo, change its visibility or gating, and rename it within its Namespace, only through the Hub API endpoints `huggingface_hub` calls (`DELETE /api/repos/delete`, `PUT /api/{type}s/{repo}/settings`, and `POST /api/repos/move` when the Namespace does not change), and only when the user is the Repo's `admin`. Transfers to another Namespace, webhooks, collaborators, access requests and every native API admin action need the `admin` scope (decided by James, 2026-10-17). `internal/auth` calls the exception `hub_admin`.
- **Personal Namespace:** the User is its `admin`.
- **Namespace profiles follow `anonymous_read`:** user and Organisation profiles, Organisation member lists, a user's likes, Namespace results in search, and public Collections are open to anyone while `anonymous_read` is on, as on the Hub, and need a signed-in caller while it is off (decided by James, 2026-10-18). Anonymous callers then get the answer they get for a public Repo: **401** on the native API (`Sign in to see users and Organisations on this Instance.`) and the Hub API, before any lookup, so the answer does not say whether a Namespace or Member exists. Lists leave the closed items out instead: search finds no users or Organisations, and Collection lists are empty. A public Collection then answers 404 to them, as a private Collection does. This covers `GET /namespaces/{ns}`, `GET /orgs/{ns}/members[/{user}]`, `GET /search`, `GET /collections[/{ns}/{slug}]` on the native API and `GET /api/users/{user}/likes` and `GET /api/collections[/{ns}/{slug}]` on the Hub API. The UI's `/{ns}` page asks a signed-out visitor to sign in.

## Local accounts

- Usernames: 2 to 42 characters, `[A-Za-z0-9][A-Za-z0-9-_.]*`, unique across Namespaces case-insensitively.
- Passwords: at least 10 characters, checked against a bundled list of common passwords, hashed with argon2id (memory 64 MiB, time 3, threads 2; parameters stored in the hash string so they can change). At most two hashes or checks run at once in a process (one for each two CPUs), so a burst of sign-ins cannot exhaust a replica's memory; the rest wait, and stop waiting when their request ends.
- Registration: `closed` (admins create users or issue invite links), `invite` (invite links only), `open`.
- Invite and reset links: single use, expire in 72 hours, created in the admin UI and copied by the admin.
- Emails: only a verified email takes part in matching (OIDC first sign-in) and in the uniqueness of `users.email`. An email an admin sets (creating a User, the admin user editor, an invite that names one) or the bootstrap admin's is verified at once, and so is a provider's email that its claims mark verified. An email a user sets for themselves, with `PATCH /me` or when registering or accepting an invite that names none, does not take effect: it is stored as the User's pending email (`email_changes`), outside `users.email` and its uniqueness, so it blocks no one's sign-in, invite or change. Hangar sends no mail, so, as for reset links, an admin creates a single-use confirmation link (`POST /admin/users/{id}/email-link`, 72 hours, a new one replaces the last) and sends it to that address. The request names the pending email the admin saw, and the link is bound to it in one transaction: if the user has changed it since, the answer is 409 and no link is made, and the response names the bound address. A new pending email ends every earlier link; confirming it (`POST /auth/email/{code}`) makes the address the User's verified email, or answers 409 if another User took it meanwhile. Used, expired and replaced links answer 404. The current address cancels a pending change and an empty one removes the email at once. Decided by the W8 8 agent after the security review of 2026-10-18; emails stored before migration `0003` are marked verified, so they keep matching.
- Login throttling: per username and per IP, with exponential backoff after 5 failures, stored in the database so it works across replicas. An attempt counts as a failure before its password is hashed, in the same transaction that checks the throttle, and a right password takes it back, so parallel guesses cannot all pass the check.
- Self-service changes (decided by James, 2026-10-18): signed-in users change their display name with `PATCH /me`, with the checks and errors of the admin user editor (at most 100 bytes), and ask for a new email there (a valid address, and 409, the admin editor's error, for an email another User has). They change their password with `POST /me/password`, which needs a UI session (every Token is refused), the current password and a new password that meets the rules above. A wrong current password counts towards the sign-in throttle for the username and IP, and a locked throttle answers 429. Success ends the User's other sessions, keeps the current one and the User's Tokens, and resets the username's throttle. A `write` Token may `PATCH /me`, as it may change the user's other `/me` settings; a `read` Token may not.

## OIDC

- Multiple providers, each with discovery, authorisation code flow with PKCE and `state` and `nonce` checks.
- Each sign-in and link is bound to the browser that started it: the start sets a short-lived cookie (`hangar_oidc`: `HttpOnly`, `Secure` when `public_url` is HTTPS, `SameSite=Lax`, path `/auth/oidc/`, ten minutes) holding a random value whose hash is stored with the `state`. The callback refuses a `state` without the matching cookie, and clears it. A link finishes only in a session of the User who started it, so a link URL forwarded to someone else cannot attach their identity, or their `admin_groups` and `org_mappings`, to another account.
- After sign-in, Hangar follows `redirect` only to a local path: one with no control characters, whitespace or backslashes, that does not start with `//`, and whose percent-decoded form passes the same checks.
- Claim mapping per provider (username, email, display name, groups).
- First login: if `auto_create_users`, create the User from the claims (de-duplicating the username with a numeric suffix if it is taken). If an existing User has the same email and it is verified ("Local accounts"), link only after that User signs in locally and confirms, never automatically; an unverified one does not block the sign-in, and the new User does not get the address.
- Display name and email are copied from the claims only when the User is created. After that they belong to the User, who can change them (`PATCH /me`; a new email waits for its confirmation link like anyone's); later logins do not overwrite them. Users created by OIDC have no password and cannot set one with `POST /me/password` (403); a local User who links a provider keeps their password.
- `admin_groups`: users in these groups are Instance admins; re-evaluated at every login.
- `org_mappings`: group membership grants Organisation Roles; re-evaluated at every login. Memberships granted by mappings are marked as such, and removed when the group goes away. Manual memberships are never touched.
- Providers with no groups claim work; mappings are then skipped.
- `local_login: false` hides the password form. The bootstrap admin can still sign in at `/auth/login?local=1`, so a broken provider cannot lock everyone out.
- Document tested setups for Keycloak, Authentik, Authelia, Microsoft Entra ID, Google, GitLab and Cloudflare Access.

## Sessions

- Cookie `hangar_session`: random 32 bytes, stored hashed, `HttpOnly`, `Secure` when `public_url` is HTTPS, `SameSite=Lax`.
- CSRF: double-submit token in a header for every state-changing native API call from the UI. Token-authenticated requests are exempt.
- Cross-origin requests: a request with a method other than `GET`, `HEAD` or `OPTIONS` that carries a credential the browser adds by itself (the session cookie, or HTTP Basic credentials) is refused with 403 when `Sec-Fetch-Site` is present and is not `same-origin` or `none`, or, without `Sec-Fetch-Site`, when `Origin` is present and names neither the request's host nor `public_url`. Password sign-in, registration, invites and resets get the same check without a cookie, so other sites cannot sign a browser in. Clients that send neither header, such as git, git-lfs, `huggingface_hub`, the CLI and Ollama, are unaffected. This follows Go's `net/http.CrossOriginProtection`.
- Users can list and revoke their sessions. Changing their password ends every session but the current one.

## Tokens

- Format: `hgr_` followed by 40 base62 characters. Stored as SHA-256; shown once.
- Scopes: `read`, `write`, `admin` (admin includes write, write includes read). A `write` Token also does the `hub_admin` action (delete a Repo, change its visibility or gating, rename it within its Namespace, through the Hub API only) when the user is the Repo's `admin`, as on huggingface.co; every other Repo administration, an Organisation's Members and the Instance need the `admin` scope ("Token scope for Repo administration" above, and [decisions](../build/decisions.md), "A write Token administers Repos through the Hub API only").
- Optional restrictions: a list of Namespaces and/or Repos.
- Optional expiry; `last_used_at` updated at most once a minute.
- Accepted as:
  - `Authorization: Bearer <token>` (Hub API, native API, Ollama token endpoint)
  - HTTP Basic password with any username, on git and git-lfs routes only. Other API routes answer 401 to Basic credentials; the static UI and health routes ignore them, because a browser that once signed in to a git URL can replay them.
- The Hub API's `whoami-v2` returns the token's name and role so `hf auth login` and `hf auth whoami` work ([03](03-hf-api.md)).
- Instance admins can list and revoke any token.

## Ollama keys

Ollama signs registry requests with the user's Ollama key pair (`~/.ollama/id_ed25519`). Users paste their public key in **Settings > Ollama keys**, as on the Hub. The Ollama auth flow is specified in [08](08-local-tools.md).

## Upstream tokens

Users can store a token per Upstream (for example their huggingface.co token) in **Settings > Upstream tokens**. Pulls they start use it, which makes gated and private upstream Repos work. Stored with AES-256-GCM under `security.instance_key`; never returned by any API after it is saved.

## Secrets at rest

`security.instance_key` encrypts upstream tokens, webhook secrets and the OIDC client secrets stored in settings. Rotation: `hangar admin rotate-key --new-key-file ...` re-encrypts everything in one transaction.

## Audit events

Record at least: logins (success and failure), logouts, token create and revoke, user create, disable and admin change, a user changing their own password (success and failure) or profile, email confirmation links created and used, Membership changes, Repo create, visibility change, rename, transfer, delete and restore, gated access decisions, Upstream and settings changes, webhook changes, and admin impersonation (not supported in v1.0; listed so nobody adds it without an audit event).
