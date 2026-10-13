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
- **Personal Namespace:** the User is its `admin`.

## Local accounts

- Usernames: 2 to 42 characters, `[A-Za-z0-9][A-Za-z0-9-_.]*`, unique across Namespaces case-insensitively.
- Passwords: at least 10 characters, checked against a bundled list of common passwords, hashed with argon2id (memory 64 MiB, time 3, threads 2; parameters stored in the hash string so they can change).
- Registration: `closed` (admins create users or issue invite links), `invite` (invite links only), `open`.
- Invite and reset links: single use, expire in 72 hours, created in the admin UI and copied by the admin.
- Login throttling: per username and per IP, with exponential backoff after 5 failures, stored in the database so it works across replicas.

## OIDC

- Multiple providers, each with discovery, authorisation code flow with PKCE and `state` and `nonce` checks.
- Claim mapping per provider (username, email, display name, groups).
- First login: if `auto_create_users`, create the User from the claims (de-duplicating the username with a numeric suffix if it is taken). If an existing local User has the same verified email, link only after that User signs in locally and confirms, never automatically.
- `admin_groups`: users in these groups are Instance admins; re-evaluated at every login.
- `org_mappings`: group membership grants Organisation Roles; re-evaluated at every login. Memberships granted by mappings are marked as such, and removed when the group goes away. Manual memberships are never touched.
- Providers with no groups claim work; mappings are then skipped.
- `local_login: false` hides the password form. The bootstrap admin can still sign in at `/auth/login?local=1`, so a broken provider cannot lock everyone out.
- Document tested setups for Keycloak, Authentik, Authelia, Microsoft Entra ID, Google, GitLab and Cloudflare Access.

## Sessions

- Cookie `hangar_session`: random 32 bytes, stored hashed, `HttpOnly`, `Secure` when `public_url` is HTTPS, `SameSite=Lax`.
- CSRF: double-submit token in a header for every state-changing native API call from the UI. Token-authenticated requests are exempt.
- Users can list and revoke their sessions.

## Tokens

- Format: `hgr_` followed by 40 base62 characters. Stored as SHA-256; shown once.
- Scopes: `read`, `write`, `admin` (admin includes write, write includes read).
- Optional restrictions: a list of Namespaces and/or Repos.
- Optional expiry; `last_used_at` updated at most once a minute.
- Accepted as:
  - `Authorization: Bearer <token>` (Hub API, native API, Ollama token endpoint)
  - HTTP Basic password with any username (git, git-lfs)
- The Hub API's `whoami-v2` returns the token's name and role so `hf auth login` and `hf auth whoami` work ([03](03-hf-api.md)).
- Instance admins can list and revoke any token.

## Ollama keys

Ollama signs registry requests with the user's Ollama key pair (`~/.ollama/id_ed25519`). Users paste their public key in **Settings > Ollama keys**, as on the Hub. The Ollama auth flow is specified in [08](08-local-tools.md).

## Upstream tokens

Users can store a token per Upstream (for example their huggingface.co token) in **Settings > Upstream tokens**. Pulls they start use it, which makes gated and private upstream Repos work. Stored with AES-256-GCM under `security.instance_key`; never returned by any API after it is saved.

## Secrets at rest

`security.instance_key` encrypts upstream tokens, webhook secrets and the OIDC client secrets stored in settings. Rotation: `hangar admin rotate-key --new-key-file ...` re-encrypts everything in one transaction.

## Audit events

Record at least: logins (success and failure), logouts, token create and revoke, user create, disable and admin change, Membership changes, Repo create, visibility change, rename, transfer, delete and restore, gated access decisions, Upstream and settings changes, webhook changes, and admin impersonation (not supported in v1.0; listed so nobody adds it without an audit event).
