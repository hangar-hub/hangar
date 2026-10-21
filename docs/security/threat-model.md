---
description: "Hangar's threat model: the assets it protects, its trust boundaries, the threats at each and how they are mitigated, for operators and contributors."
---

# Threat model

This page describes what Hangar protects, where its trust boundaries are, the threats at each boundary, and how each threat is mitigated or why it remains. It is written for operators who decide how to deploy Hangar, and for contributors who change code near a boundary.

Every mitigation on this page names the code that implements it and the test that keeps it in place. Where no test exists, or a threat is accepted, the page says so under **Residual risk**. Paths are relative to the repository root. The page reflects the `integration/v1` branch after the phase 3 security review (October 2026).

For deployment advice, see [Security hardening](../operate/security.md). To report a vulnerability, see [Report a vulnerability](#report-a-vulnerability).

## Scope

In scope:

- The `hangar` binary in each process role (web server, worker, and the `hangar` CLI and `hangar admin` commands).
- The container image, the Helm chart and the Compose examples in `deploy/`.
- The protocols Hangar serves: the web UI and native API, the Hugging Face Hub API (including Xet), git smart HTTP and git-lfs, the Ollama registry and the Dataset Viewer API.
- The connections Hangar makes: to Upstreams, webhook receivers, OIDC providers, the database and the Blob store.
- The build and release pipeline in `.github/workflows/`.

Out of scope, as in the [security policy](../project/security.md):

- Malicious content in models and datasets that users store. Hangar never loads or runs stored files, and it scans pickle-based files and can refuse to serve them (see [The download gate](#the-download-gate)), but it cannot make an unsafe file safe to load.
- Denial of service through traffic volume. Put a reverse proxy with rate limits in front of Hangar.
- The security of the host, Kubernetes cluster, database server, object store and identity provider that an operator runs Hangar on.

## Assets

| Asset | Why it matters | Where it lives |
|---|---|---|
| Private and gated Repos | Their files, names and history must reach only people with access. | Git data and LFS objects in the Blob store; metadata in the database. |
| Integrity of Repos | Clients trust that a file at a Commit is what was uploaded, and that a Mirror matches its Upstream. | git objects (SHA-1), LFS objects (addressed by SHA-256). |
| Users' machines | A pickle-based model file can run code when a client loads it. | The client; Hangar is the distribution point. |
| Tokens, sessions and passwords | Each lets its holder act as a User. | The database holds only hashes: SHA-256 for Tokens and session IDs (`internal/auth/tokens.go`, `sessions.go`), argon2id for passwords (`internal/auth/passwords.go`). |
| The Instance key | Encrypts stored secrets and derives the URL-signing key. | Configuration or `secrets.yaml` in `server.data_dir`; never in the database. |
| Stored secrets | Upstream tokens, webhook secrets and OIDC client secrets give access to other systems. | The database, encrypted with AES-256-GCM under the Instance key (`internal/secrets/secrets.go`). |
| Infrastructure credentials | The database DSN and S3 keys give full access to an Instance's data. | Configuration, environment or Kubernetes Secrets. |
| The audit log | The record of who did what. | The database, also exported as OpenTelemetry log records. |
| Availability | One user should not be able to stop an Instance serving everyone else. | Web and worker processes, the database and the Blob store. |

## Actors

- **Anonymous caller:** anyone who can reach the Instance over the network.
- **Signed-in User:** with `open` or `invite` registration, anyone who gets an account. Every User administers their own Namespace, so they can create Repos, push, add webhooks and start Pulls into it.
- **Organisation Member and collaborator:** access to other Namespaces' Repos through a Role.
- **Instance admin:** manages Upstreams, settings, quotas and users. Trusted.
- **Operator:** runs the deployment, holds the configuration, the Instance key and infrastructure credentials. Trusted.
- **Malicious model author:** uploads, or publishes on an Upstream, a file built to run code on whoever loads it.
- **Hostile or compromised Upstream:** a Hub that Hangar Pulls from and that answers with crafted data or redirects.
- **Cross-site attacker:** a web page that a signed-in user visits in the same browser.
- **Compromised dependency or build step:** code that reaches the binary or image through the supply chain.

## Trust boundaries

```text
              Browser                CLI, huggingface_hub,           Ollama
          (UI, native API)          hf, git, git-lfs, Xet          (registry)
                 |                           |                          |
=================|== 1 ======================|== 2, 3 ==================|== 4 ====
                 v                           v                          v
          +------------------------------------------------------------------+
          |  Web process: authenticate, authorise, gate downloads            |
          +------------------------------------------------------------------+
               |            |                 |                 |
         ==== 8 ====   ==== 7 ====       ==== 5 ====       ==== 6 ====
               v            v                 v                 v
           Database     Blob store        Upstreams         Webhook receivers
               ^         (presigned       (outbound)          (outbound)
               |          URLs to
         ==== 9 ====      clients)
               |
          Worker process: Pulls, scan, index, Parquet export, webhook
          delivery; it also crosses 5, 6 and 7

 10: operators, configuration and `hangar admin` commands, across all of the above
```

1. **Browser:** the web UI and the native API it calls, with the session cookie.
2. **CLI and Hub clients:** the Hub API and native API, with Tokens as bearer credentials.
3. **git, git-lfs and Xet:** smart HTTP, the LFS batch API and signed object URLs, and the Xet CAS.
4. **Ollama:** the registry API, with Ollama key signatures and registry tokens.
5. **Upstreams:** outbound requests to other Hubs, which answer with untrusted data.
6. **Webhooks:** outbound POSTs to addresses that Namespace admins choose.
7. **The Blob store and presigned URLs:** object storage that clients may reach directly.
8. **The database:** every row of state, shared by all processes.
9. **The worker:** processes that parse untrusted files and act for Users after the request that started the work has ended.
10. **Operators:** configuration, secrets and administrative commands.

## Threats by boundary

The tables use STRIDE: **S**poofing, **T**ampering, **R**epudiation, **I**nformation disclosure, **D**enial of service and **E**levation of privilege. Tests are Go tests unless named otherwise.

### Every route

Two checks apply to every boundary that accepts requests:

- **Authorisation on every route.** Every handler calls `auth.Authz.Can` (`internal/auth/authz.go`). `TestRouteAuthorisationMatrix` (`cmd/hangar/routes_authz_test.go`) finds every route of every router (the native API with `chi.Walk`, the others by parsing the routing packages), fails when a route has no entry in its permission matrix, and probes all 222 routes on a full server as six callers. A new route cannot ship without an authorisation decision.
- **Private Repos stay hidden.** A caller who cannot read a private Repo gets 404, as for a Repo that does not exist, on every protocol. The route matrix checks this for every Repo route; `TestPrivateDatasetsAreHidden` (`internal/datasets/viewer/viewer_test.go`) covers the Viewer.

### 1. Browser

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| S | A cross-site page makes a signed-in browser change state (CSRF) with the session cookie. | `SameSite=Lax` cookie, a double-submit CSRF header on every state-changing call, and a cross-origin check on `Sec-Fetch-Site` and `Origin` (`internal/auth/crossorigin.go`, `authenticator.go`). | `TestAuthenticateSessionsAndCSRF`, `TestCrossOriginBrowserCredentials` (`internal/auth/`). |
| S | A browser that once opened a git URL caches HTTP Basic credentials and replays them on cross-site requests. | Basic credentials are accepted only on git and git-lfs routes, and the cross-origin check applies to them too. | `TestBasicCredentialsOnlyForGit`, `TestCrossOriginBrowserCredentials` (`internal/auth/crossorigin_test.go`). |
| S | Login CSRF: another site signs the browser in to an attacker's account. | Sign-in, registration, invite and reset calls get the cross-origin check without a cookie. | `TestCrossOriginBrowserCredentials` (`internal/auth/crossorigin_test.go`), which includes sign-in calls. |
| S | Password guessing, including a burst of parallel guesses that all pass the throttle before the first failures are written. | Throttling per username and per IP, stored in the database so it holds across replicas. Each attempt counts as a failure, in the same transaction as the throttle check, before its password is hashed, and only a right password takes it back. A common-password list; argon2id hashing (`internal/auth/login.go`, `passwords.go`). | `TestPasswordLoginAndThrottling`, `TestLoginThrottleHoldsUnderParallelAttempts`, `TestPasswordPolicy` (`internal/auth/login_test.go`). |
| S | OIDC sign-in is forged or replayed. | Authorisation code flow with PKCE, `state` stored hashed and `nonce` checked (`internal/auth/oidc.go`); an existing account with the same email is linked only after its owner signs in locally (`ErrOIDCLinkRequired`). | `TestOIDCKeycloak` (`internal/auth/oidc_integration_test.go`), which also refuses a reused or made-up `state` and a tampered code, and checks the link rule. |
| S | OIDC login CSRF, or a forwarded link URL: a callback started in one browser is finished in another, signing the victim in to the attacker's account, or attaching the victim's identity, `admin_groups` and `org_mappings` to the attacker's account. | Each flow is bound to the browser that started it by the `hangar_oidc` cookie, whose hash is stored with the `state`; a link also needs a session of the User who started it (`internal/auth/oidc.go`, `handler.go`). | `TestOIDCBrowserBinding` (`internal/auth/oidc_binding_test.go`), `TestOIDCKeycloak`. |
| S | An open redirect after OIDC sign-in sends the user to a look-alike site. | `SafeRedirect` keeps only local paths and refuses control characters, whitespace and backslashes, which browsers remove or read as `/` (`internal/auth/oidc.go`). | `TestSafeRedirect` (`internal/auth/oidc_claims_internal_test.go`). |
| T | Stored cross-site scripting through a Model card, discussion or file. | Markdown is sanitised with GitHub's schema (no scripts, event handlers or iframes) in `ui/src/components/content/Markdown.tsx`; external links get `noopener noreferrer`. Files are served as attachments with `nosniff` (`internal/blob/download.go`). A strict CSP blocks inline scripts. | Vitest "removes scripts, event handlers and iframes" (`ui/src/components/content/Markdown.test.tsx`); `TestServesIndexForUIRoutesWithCSP` (`internal/web/web_test.go`). |
| T | Clickjacking. | `frame-ancestors 'none'` and `X-Frame-Options: DENY` (`internal/web/web.go`). | `TestServesIndexForUIRoutesWithCSP`. |
| I | Session theft over plain HTTP or by script. | Cookie is `HttpOnly`, and `Secure` when `server.public_url` is HTTPS (`internal/auth/sessions.go`). | `TestSecureCookiesOnHTTPS` (`internal/auth/authenticator_test.go`). **Residual risk:** Hangar sends no HSTS header; the TLS proxy should. |
| I | External images in a card track who views it. | `ui.external_images` can turn them off. | Vitest "does not load external images when the Instance turns them off". |
| D | A burst of sign-ins exhausts memory (each argon2id check holds 64 MiB). | At most two hashes run at once per process; the rest wait and give up with their request (`internal/auth/passwords.go`). | `TestPasswordHashingIsBounded`, `TestPasswordHashingWaitEndsWithContext` (`internal/auth/passwords_internal_test.go`). **Residual risk:** a flood still delays sign-ins on that replica; Hangar has no per-IP limit before the hash. Rate-limit `/auth/login` at the proxy. |
| S | Someone with a user's session (an unlocked browser, a stolen cookie) changes the password and locks the owner out, or guesses the password through the change form. | `POST /me/password` needs the current password, the CSRF header and a same-origin request. Wrong current passwords count towards the sign-in throttle for the username and IP, so the form is no faster to guess through than sign-in. Success and failure are audited (`user.password_changed`, `user.password_change_failed`) (`internal/auth/account.go`). | `TestChangePassword` (`internal/auth/account_test.go`), `TestChangeMyPassword`, `TestChangeMyPasswordIsThrottled` (`internal/api/account_test.go`), `TestSelfServiceAccountOnTheFullServer` (`cmd/hangar/account_test.go`). **Residual risk:** wrong current passwords also lock the user's own sign-in for a while, as failed sign-ins do. |
| S | After a password change, a session or Token that someone else holds keeps working. | The change ends every session of the user but the current one. | `TestChangeMyPassword`, `TestSelfServiceAccountOnTheFullServer`. **Residual risk:** Tokens are kept, so automation does not break; a user who thinks their account was used must revoke their Tokens too ([Security hardening](../operate/security.md)). |
| S | A user sets their email to someone else's address to squat it: blocking that person's OIDC first sign-in (`ErrOIDCLinkRequired`), invite or change, or claiming the address before its owner. | A self-set email (`PATCH /me`, registration) is only pending (`email_changes`), outside `users.email` and its uniqueness, until a single-use, 72-hour confirmation link that an admin creates and sends to that address is used. The link is bound to the pending email the admin saw, in one transaction, so a change the user makes before the admin approves gives 409 rather than a link for another address, and a new pending email ends every earlier link; only verified emails match OIDC sign-ins. Admin-set, invite, bootstrap and verified-claim emails are verified at once (`internal/auth/account.go`, `oidc.go`, `invites.go`). | `TestSelfSetEmailsNeedConfirmation` (which also covers the binding), `TestRegistrationEmailsNeedConfirmation` (`internal/auth/account_test.go`), `TestUpdateMe` (`internal/api/account_test.go`), `TestSelfServiceAccountOnTheFullServer`. **Residual risk:** Hangar sends no mail, so the proof of ownership is only as good as the admin sending the link to the right address; an admin who sets an email vouches for it. Emails stored before migration `0003` were marked verified, including any typed at open registration. |
| E | A Token mints a longer-lived Token, or a Token links an OIDC identity, approves a device login or changes a password. | An expiring Token can create only Tokens that expire no later; linking a provider, approving a device and changing a password need a UI session. A `write` Token may change only the display name and email (`PATCH /me`). | `TestSessionsTokensAndKeys`, `TestChangeMyPassword` (`internal/api/`); `TestRouteAuthorisationMatrix`. |

### 2. CLI and Hub clients

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| S | A stolen Token is used. | Tokens are stored as SHA-256, shown once, can expire and be limited to Namespaces or Repos, and can be revoked by the user or an Instance admin. `last_used_at` shows use. | `TestAuthenticateTokens`, `TestTokenValidation` (`internal/auth/authenticator_test.go`); `TestPermissionMatrix` (`internal/auth/authz_test.go`). |
| E | A Token does more than its scope allows. | Scopes `read`, `write` and `admin`, applied by `auth.Authz` on top of the user's Role (see [Token scopes](#token-scopes)). | `TestPermissionMatrix` (`internal/auth/authz_test.go`), `TestWriteTokenAdministersReposThroughHubAPIOnly` (`cmd/hangar/hub_admin_test.go`), compat `test_write_token_administers_own_repos` (`compat/hf/test_repos.py`). |
| E | A rename that follows an old Repo ID's redirect into another Namespace turns into a transfer. | `hub_admin` checks the Namespace the Repo is in now. | `TestWriteTokenAdministersReposThroughHubAPIOnly` (redirect case). |
| T | A path escapes the Repo (`../`, absolute, backslash, NUL) or names `.git` in a spelling a client checks out as `.git` (CVE-2014-9390). | `model.CleanRepoPath` and `model.ValidPathComponent` port git's NTFS and HFS+ checks and limit paths to 4,096 bytes (`internal/model/entry.go`). | `TestCleanRepoPath`, `TestValidPathComponent` (`internal/model/model_test.go`). **Residual risk:** paths that differ only in case or Unicode normalisation can coexist, as on the Hub. |
| T | A zip export entry lands outside the export folder. | Every entry must pass `CleanRepoPath` unchanged, or the Job fails (`internal/repo/jobs.go`). | `TestZipExportRefusesUnsafePaths` (`internal/repo/treecheck_test.go`). |
| T | A folder uploaded with `hangar push` or `hangar import` holds a symbolic link out of it, such as to `~/.ssh`. | Links are followed only into the folder or the Hugging Face cache (`internal/cli/symlinks.go`). | `TestPushSkipsSymlinksOutOfTheFolder`, `TestWithin`, `TestSymlinksToCacheBlobsAreFollowed` (`internal/cli/symlinks_test.go`). |
| I | A user learns that an LFS or Xet object exists, or downloads it, by committing a pointer to its hash. | Objects stay stored once per Instance, but a Commit, push or pull request may reference one only when its author uploaded the bytes (`lfs_uploaders`) or can read a Repo that holds it (`repo.Store.Referable`, `internal/repo/references.go`). Anything else is refused as an object that was never uploaded. The batch API, the native upload plan and verify ask for the bytes of such an object as if it did not exist, never giving its size; an upload of bytes already stored is checked and discarded, with no second copy (decided by James on 2026-10-18). | `TestLFSObjectsOfPrivateReposCannotBeReferenced` (`cmd/hangar/lfs_references_test.go`: batch, Hub API commit and `create_pr`, git push and Xet, a reader who may reference it, and uploads through git-lfs and Xet that succeed); `TestCommitsReferenceOnlyObjectsTheActorUploadedOrCanRead`, `TestUploadsOfStoredObjectsProveTheBytes` (`internal/repo/references_test.go`); `TestLFSBatchAsksForObjectsTheCallerCannotUse` (`internal/gitserver`); `TestCommitsCannotReferenceObjectsOfPrivateRepos` (`internal/hfapi`); `TestXetUploadsRecordUploadersAndRevealNothing` (`internal/xet`); compat `TestLFSObjectsOfOtherReposNeedTheirBytes` (`compat/git`), `test_upload_of_an_object_stored_in_another_users_private_repo` (`compat/hf`, with and without Xet), `test_file_stored_in_another_users_private_repo` (`compat/xet`). **Residual risk:** see [LFS object references](#lfs-object-references). |
| I | Usernames and Roles on a closed Instance are listed through Namespace profiles, Organisation member lists, a user's likes, Namespace search results or public Collections. | While `anonymous_read` is off, reading a Namespace needs a signed-in caller (`Authorizer.canSeeNamespace` in `internal/auth/authz.go`), checked before any lookup, so anonymous callers get 401 whether or not the Namespace exists; search and Collection lists leave the closed items out (`internal/api/namespaces.go`, `orgs.go`, `search.go`, `internal/hfapi/likes.go`, `internal/collections/service.go`). Decided by James on 2026-10-18. | `TestNamespacesNeedSignInWhileAnonymousReadIsOff` (`cmd/hangar/namespace_access_test.go`), `TestNamespaceReadFollowsAnonymousRead` (`internal/auth/authz_test.go`), and the `profile` rule of `TestRouteAuthorisationMatrix` (`cmd/hangar/routes_authz_test.go`), which probes each profile route with the setting off and on. **Residual risk:** every signed-in user, whatever their Roles, can still list every username and Organisation through search and profiles; with `anonymous_read` on, they are public, as on the Hub. |
| D | Large or many requests exhaust a replica. | Request bodies and batches are bounded per protocol (below). | **Residual risk:** no per-user or per-IP request rate limit; use the proxy. |
| R | A user denies a change. | Security-relevant actions are audited: sign-ins, Tokens, Members, Repo visibility, rename, transfer and delete, settings (`internal/audit`). | `TestEveryW5ActionIsAudited` (`internal/api/identity_test.go`) for sign-ins, Tokens, users, invites and Members; `TestCreateAndSettings` and `TestRenameAndTransferRedirect` (`internal/repo/lifecycle_test.go`) for visibility, renames and transfers. |

### 3. git, git-lfs and Xet

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| D | Delta bomb: a 16 KiB pack expands to gigabytes in memory before any hook runs. | The pack is checked as it streams, and its trailer held back until every object and delta is within limits (`internal/gitstore/packlimit.go`, `internal/gitserver/receive.go`). | `TestPushRefusesDeltaBomb` (`internal/gitserver/limits_test.go`), `TestLimitPackRefusesHostilePacks` (`internal/gitstore/packlimit_test.go`). |
| D | Tree fan-out bomb: a 666-byte pack names millions of paths. | Each distinct tree is read once; Commits over 1,000,000 paths or with a path over 4,096 bytes are refused (`internal/repo/treecheck.go`). | `TestApplyPackRefusesTreeBombs`, `TestApplyPackPathLimitIsConfigurable` (`internal/repo/treecheck_test.go`). |
| D | A push with endless ref updates, objects or malformed data. | 10,000 ref updates, 2,000,000 objects and four times `git.max_pack_size` unpacked per push; malformed packs are refused. | `TestPushRefusesTooManyRefUpdates`, `TestPushRefusesMalformedPack` (`internal/gitserver/limits_test.go`). **Residual risk:** no limit on concurrent pushes per user, and clone memory is git's (a replica peaked at 496 MiB in four parallel clones of a 10,000-Commit Repo). |
| D | Oversized LFS batch requests. | 1,000 objects and a 4 MiB body per batch (`internal/gitserver/lfs.go`). | `TestLFSRequestErrors` (`internal/gitserver/lfs_errors_test.go`), `FuzzParseBatch` (`internal/gitserver/fuzz_test.go`). |
| D | Xet shards that make the server reload indexes without end (CPU amplification). | Index reloads are paid for by what a shard names (`internal/xet/indexcache.go`). | `TestShardIndexMemoryIsBounded` (`internal/xet/index_limits_test.go`), `TestIndexReloadAllowance`; `FuzzParseXorb`, `FuzzParseShard` (`internal/xet/fuzz_test.go`). |
| S | A signed LFS or Xet URL is forged, reused for another object or extended. | HMAC-SHA256 over the path, scope, expiry and every query parameter, with a key derived from the Instance key by HKDF, compared in constant time; keys and values are escaped before signing (`internal/httpx/signer.go`). Xet CAS tokens carry a scope and an expiry. | `TestSigner` (`internal/httpx/httpx_test.go`), `TestCASScopesExpiryAndReject` (`internal/xet/cas_test.go`). |
| I | The git-lfs batch, verify or a Xet shard answer says whether an object exists, or its size. | Objects the caller may not reference get upload actions like missing ones; verify answers 404 until the caller's own upload is checked; Xet errors give no stored size, and a xorb Hangar has is verified before `was_inserted: false` is answered. | `TestLFSBatchAsksForObjectsTheCallerCannotUse` (`internal/gitserver/references_test.go`), `TestXetUploadsRecordUploadersAndRevealNothing` and `TestAcceptShardRejects` (`internal/xet/service_test.go`). |
| T | An upload replaces a verified object, or stores bytes that do not match their SHA-256. | Uploads go to a staging key and are verified before they move into place (`internal/repo/`). | `TestPresignedPutVerifyRejectsWrongContent`, `TestPresignedPutCannotReplaceAVerifiedObject` (`internal/repo/presign_test.go`). |
| E | A write Token force-pushes the default branch. | Allowed only for a Namespace admin, as spec 04 says (`gitserver.canForcePush`). | **Residual risk:** for that one check the Role decides, not the Token scope, so a Namespace admin's write Token can force-push. |
| I | A pickle stored in git rather than LFS leaves through `git clone`. | `scan.Gate.CheckGit` runs before every fetch (`internal/gitserver/smart.go`). | `TestGateChecksGitFetches` (`internal/scan/bypass_test.go`); see [The download gate](#the-download-gate). |

### 4. Ollama

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| S | A registry request is forged or replayed. | Requests are signed with the user's registered Ed25519 key; timestamps outside 5 minutes and nonces seen in the last 10 minutes are refused (`internal/auth/ollama.go`). | `TestOllamaKeysAndRegistryTokens` (`internal/auth/keys_test.go`), `FuzzParseOllamaAuthorization`. |
| I | Private Repos leak through the registry. | The key's owner must be able to read the Repo; others get 404. | `TestPrivateReposUseTheOllamaKeyFlow` (`internal/ollama/registry_test.go`), `TestRouteAuthorisationMatrix`. |
| E | A restricted or expiring Token registers an Ollama key whose registry tokens read every Repo the user can, for as long as the account exists. | A key added with a Token is bound to it: registry tokens get the Token's scope and restrictions, end no later than the Token, and are refused once it is revoked or has expired (`internal/auth/ollama.go`). | `TestOllamaKeysFollowTheirToken` (`internal/auth/keys_test.go`). |
| I | An unsafe file is pulled as a layer. | Blob routes and their signed redirects call the download gate. | `TestUnsafeDownloadGateCoversEveryRoute` (`cmd/hangar/unsafe_test.go`). |
| T | A browser renders a blob as a page. | Blobs are `application/octet-stream`, which browsers do not render. | **Residual risk:** blob responses do not send `X-Content-Type-Options: nosniff` (`internal/ollama/registry.go`, `serve`), unlike every other route that serves file bytes. |

### 5. Upstreams

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| I, E | SSRF: an Upstream redirects Hangar to the cloud metadata service or the local network. | The [network policy](#ssrf-policy), applied in the dialler to the resolved address (`internal/upstream/netpolicy.go`, `internal/httpx/netaddr.go`). | `TestUpstreamRedirectsCannotReachTheLocalNetwork`, `TestUpstreamNeverReachesReservedAddresses`, `TestCheckAddr` (`internal/upstream/netpolicy_internal_test.go`). |
| I | Credentials are sent to another origin on a redirect. | `Authorization` and `Cookie` are dropped when the scheme, host or port changes (`internal/upstream/client.go`). | `TestHTTPClientRepoInfoTreeDownload` (`internal/upstream/client_test.go`); `TestCheckRedirectDropsTheTokenAcrossOrigins` (`internal/cli/redact_test.go`) for the `hangar` CLI. |
| D, T | An Upstream sends hostile packs, trees, names or endless ref advertisements. | Pulls go through the same pack and tree checks as pushes, refuse names a Repo cannot hold, and read at most 100,000 refs (`internal/upstream/gitfetch.go`). | `TestApplyPackRefusesDeltaBombs`, `TestApplyPackRefusesTreeBombs` (Mirror cases), `TestFetchPackBoundsAdvertisedRefs` (`internal/upstream/gitfetch_internal_test.go`). |
| D | Pulls in one Namespace starve another's. | Each Pull queues its own transfer and quotas are per Namespace (`internal/upstream/plan.go`). | `TestPullQuotaOfAnotherNamespaceCannotStarveAPull` (`internal/upstream/quota_test.go`). |
| T | A malicious model is pulled from an Upstream. | Pulled Commits are scanned like pushed ones, and the download gate applies to Mirrors and Pull-through. | `TestPullThroughURLChecksUnsafeDownloads` (`internal/upstream/pullthrough_gate_test.go`), `TestGateRefusesPartialMirrorPickleByOID` (`internal/scan/gate_test.go`). |
| E | Anonymous callers fill the Blob store through Pull-through. | Pull-through is off by default, limited by an allow list, and refused to anonymous callers unless `pull.pull_through.min_role` is `anonymous` (`internal/upstream/pullthrough.go`). | `TestPullSettingsFollowInstanceSettings` (`cmd/hangar/instance_test.go`). **Residual risk:** no test refuses an anonymous Pull-through at the default role. |

**Residual risk:** an Upstream whose own DNS an attacker controls can resolve to a private address, because that is how LAN Upstreams work; admins choose every Upstream. A proxy set with `HTTP_PROXY` or `HTTPS_PROXY` decides what Upstream requests reach.

### 6. Webhooks

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| I, E | SSRF: a Namespace admin (every user, for their own Namespace) points a webhook at the metadata service, the local host or the LAN. | Reserved addresses are always refused, checked on the dialled address; redirects are not followed (`internal/webhook/service.go`). Private addresses, loopback included, are refused unless an admin turns on `webhooks.allow_private_targets`, which is off by default (`internal/auth/settings.go`). | `TestReservedTargetsAreAlwaysRefused`, `TestTestDeliveryAndPrivateTargets` (`internal/webhook/service_test.go`); `TestSettings` (`internal/auth/users_test.go`) checks the default. **Residual risk:** with the setting on, users can POST to LAN services and read the status code. |
| S | A receiver accepts a forged delivery. | Each delivery is signed with the webhook's secret (`internal/webhook/service.go`); secrets are stored encrypted. | `TestEmitSignsAndDelivers` (`internal/webhook/service_test.go`), `TestSignAndVerify` (`internal/webhook/webhook_test.go`). |
| E | A write Token adds a webhook to watch private changes. | Webhooks need the `admin` scope. | `TestWriteTokenAdministersReposThroughHubAPIOnly`. |

### 7. The Blob store and presigned URLs

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| I | A presigned URL outlives the holder's access. | URLs are short-lived: `storage.s3.presign_ttl` is 1 hour by default and validation keeps it between 1 minute and 7 days (`internal/config/validate.go`). | **Residual risk:** a presigned S3 URL is scoped to the object key (its SHA-256), not the Repo, and cannot be revoked before it expires. Keep the TTL short. |
| I | A presigned URL issued before the gate would refuse the file. | Hangar issues a redirect only after the gate allows the file. | `TestResolveHeadIssuesNoPresignedLinkPastTheGate` (`internal/hfapi/unsafe_test.go`). **Residual risk:** in `redirect` mode the check happens at issue, not at use. |
| I | Presigned URLs leak into logs. | Clients and the Upstream client redact them. | `TestHTTPClientRedactsPresignedURLs` (`internal/upstream/client_test.go`), `TestPullDoesNotPrintPresignedDownloadURLs` (`internal/cli/redact_test.go`), `TestSecretsStayOutOfLogsAndTelemetry`. |
| T | An object is changed in the bucket. | Objects are addressed by SHA-256 and verified on upload. | **Residual risk:** objects are not re-hashed on download; anyone with write access to the bucket can change them. Restrict the bucket to Hangar's credentials. |

Lifetimes of the URLs Hangar signs itself:

| URL | Scope | Lifetime |
|---|---|---|
| LFS download | One object in one Repo | `storage.s3.presign_ttl` (1 hour) |
| LFS upload, verify and multipart part | One object or upload in one Repo | `lfs.upload_url_ttl` (24 hours) |
| Zip export | One export, gated again when used | 24 hours (`repo.ExportTTL`) |
| Pull-through | One object of one Mirror | 1 hour |
| Ollama blob | One digest of one Repo | 1 hour |
| Dataset Viewer asset | One asset path | 1 hour |

Hangar-signed links for LFS, Pull-through and zip exports run the download gate again when they are used.

### 8. The database

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| T, I | SQL injection. | Queries use bun with bound parameters; where SQL text is assembled, the pieces are constants in the code (for example `internal/gc/usage.go`, `internal/org/db.go`) and caller values are bound. The Dataset Viewer's `where` and `orderby` are parsed by Hangar's own grammar, not passed to a SQL engine (`internal/datasets/query`). | `gosec` in `make lint` flags SQL built from strings. `TestWhere`, `FuzzWhere`, `FuzzOrderBy` (`internal/datasets/query/query_test.go`). **Residual risk:** no test feeds hostile names through the database layer itself. |
| I | A database dump exposes credentials. | Tokens and session IDs are stored as hashes, passwords as argon2id, and stored secrets encrypted under the Instance key, which is not in the database. | `TestRotateKeyReencryptsSecrets` (`internal/cli/admin_test.go`), `TestSealOpen` (`internal/secrets/secrets_test.go`). |
| E | Write access to the database. | None in Hangar: rows decide every permission. | **Residual risk:** database write access is full control of the Instance. Restrict network access to the database, and use TLS in the DSN when it is on another host. |

### 9. The worker

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| E | A crafted file runs code during scanning or indexing. | The scanner reads pickle opcodes and never unpickles; no stored file is ever executed. | `FuzzScan` (`internal/scan/pickle/pickle_test.go`), `FuzzScanFile` (`internal/scan/service_test.go`). |
| D | Crafted GGUF, safetensors, Parquet, Arrow, CSV, JSON Lines, archives or cards exhaust the worker. | Parsers have size limits and are fuzzed nightly (`.github/workflows/fuzz.yml`, `scripts/fuzz.sh`); scan strings and frames are capped at 64 MiB; Viewer scans stop at their limits and report partial results. | `FuzzRead` (`internal/index/gguf`, `internal/index/safetensors`), `FuzzOpenAndRead` (`internal/datasets/pq`), `FuzzCSV`, `FuzzJSONLines` (`internal/datasets/convert`), `FuzzZipEntries` (`internal/datasets/layout`), `TestConvertShardsAndLimits` (`internal/datasets/convert`), `TestScanLimitsMakeResultsPartial` (`internal/datasets/viewer`). |
| D | A compression bomb in a dataset file: a 67 KB `.zst` file whose frame asks for a 512 MiB window, or a small gzip, bzip2 or zstd file of blank lines that inflates without end. | zstd windows over 128 MiB (the zstd command line's own limit) are refused before the decoder holds anything, in the converter and the scanner; every decompressed stream in the converter stops at 64 GiB with an error (`internal/datasets/convert/source.go`, `internal/scan/files.go`). | `TestDecompressRefusesLargeZstdWindows`, `TestDecompressStopsAtTheByteLimit` (`internal/datasets/convert/decompress_internal_test.go`). |
| E | A user keeps control of Jobs, or their results such as zip exports, after losing access to the Repo. | Job routes check the caller's current access to the Job's Repo: a user demoted to `read` cannot cancel or retry, and a removed user gets 404 for the Job and its export. | `TestJobsFollowRevokedRepoAccess` (`internal/api/jobs_test.go`). |
| I | A failed Job hides an unsafe file. | A read error fails the scan Job, which retries; files with no result are refused when blocking is on. | `TestGateAfterAFailedScanJob` (`internal/scan/failed_scan_test.go`), `TestGateOverHTTPAfterAFailedScanJob` (`internal/scan/gate_failed_test.go`). |

**Residual risk:** the worker holds the same credentials as the web process (database, Blob store, Instance key) and decrypts Upstream tokens to use them. It runs `git index-pack` on Upstream data, bounded by the pack check.

### 10. Operators

| | Threat | Mitigation | Evidence or residual risk |
|---|---|---|---|
| I | Example deployments ship with default secrets. | Compose files have no default secrets; Helm examples hold no credentials; missing secrets are generated once into `secrets.yaml`. | `TestComposeSecretsHaveNoDefaults`, `TestHelmExampleValuesHoldNoCredentials` (`deploy/deploy_test.go`), `TestEnsureSecretsGeneratesOnceThenReads` (`internal/config/config_test.go`). |
| I | Support services are exposed on every interface. | Only Hangar listens on every interface in the Compose files. | `TestComposeOnlyHangarListensOnEveryInterface` (`deploy/deploy_test.go`). |
| I | Secrets appear in logs, spans or metrics. | Access logs and spans record route templates, never raw paths or queries; outbound URLs are redacted. Server spans carry the raw path as `url.path`, so invite, reset and email confirmation paths reach the tracing middleware with the code replaced by `{code}` (`httpx.HideSecretPaths`). | `TestSecretsStayOutOfLogsAndTelemetry` (`cmd/hangar/secrets_telemetry_test.go`) runs a full server at debug level with OTLP export and checks that 19 kinds of secret never appear, including the three path codes; `TestHideSecretPathsKeepsCodesOffSpans` (`internal/httpx/secretpaths_test.go`). |
| T | A leaked Instance key. | `hangar admin rotate-key` re-encrypts every stored secret in one transaction. | `TestRotateKeyReencryptsSecrets`. Rotation also invalidates signed URLs. |
| E | The container is used to attack the host. | The image runs as UID 65532 with no shell; the chart drops every capability, uses `RuntimeDefault` seccomp, a read-only root filesystem and no privilege escalation (`Dockerfile`, `deploy/helm/hangar/values.yaml`). | **Residual risk:** no test asserts the chart's security context. The NetworkPolicy is off by default and allows all egress when on, because Hangar talks to Upstreams, OIDC providers and webhook receivers. |

## The download gate

Pickle-based files (PyTorch, joblib, NumPy object arrays and raw pickles) can run code when a client loads them. Hangar scans them and, with `scan.block_unsafe_downloads` on, refuses to serve risky ones. The setting is off by default. `scan.block_level` sets how strict it is: `unsafe` (the default) or `suspicious`. The rules for users are in [Blocking unsafe downloads](../operate/security.md#blocking-unsafe-downloads).

### One gate for every route

Every route that serves a Repo's file bytes calls `scan.Gate` (`internal/scan/gate.go`): `CheckFile` for paths, `CheckTree` for zip exports, `CheckObjects` for objects requested by OID (git-lfs, Pull-through, Ollama) and `CheckGit` for git fetches. The gate refuses a file that can run code when:

- The scan marked it unsafe (`BlockUnsafe`).
- The scan marked it suspicious and `scan.block_level` is `suspicious` (`BlockSuspicious`).
- It has no scan result yet, because the scan is running or its Job failed (`BlockNotScanned`).
- The scan could not follow it to where every loader stops (`BlockPartial`).
- The history is too long to check (`BlockHistory`).

The Hub API and web UI accept `accept_risk=1`; git-lfs and Ollama answer 403 with the reason.

| Test | What it keeps true |
|---|---|
| `TestServingSurfaceIsClassified` (`cmd/hangar/unsafe_surface_test.go`) | Enumerates the router's protocols, its signed-link routes and every function that reads a Repo file's bytes, and fails on any that is not classified. |
| `TestUnsafeDownloadGateCoversEveryRoute` (`cmd/hangar/unsafe_test.go`) | Every download route refuses an unsafe file. |
| `TestUnsafeDownloadGateCatchesScannerBypasses` (`cmd/hangar/unsafe_bypass_test.go`) | Each known bypass is refused through the full server. |
| `TestSuspiciousDownloadsFollowTheBlockLevel` (`cmd/hangar/unsafe_level_test.go`) | Every download route serves a suspicious pickle at level `unsafe` and refuses it at level `suspicious`, where `accept_risk=1` still works on the Hub API. |
| `TestGateRefusesUnsafeAndUnscannedFilesThatRunCode`, `TestGateBlockLevel`, `TestGateRefusesPartialScans`, `TestGateChecksEveryCommitThatHoldsAnObject`, `TestGateChecksObjectsOfTags`, `TestGateChecksGitFetches` (`internal/scan/`) | The gate's rules, including objects held only by older Commits or tags. |

### How the scanner reads files

The scanner (`internal/scan/pickle`) walks the opcode stream the way CPython's unpickler does and classifies every import against allow and deny lists (`internal/scan/pickle/lists.go`). Content decides how a file is read: it decompresses, opens zip archives and `.npz` members, and follows the array data joblib stores inside its pickles. Files that fail at their first opcode are not pickles and are safe.

Bypasses fixed in phase 3, each with a regression test in `internal/scan/bypass_test.go`, `internal/scan/pickle/bypass_test.go`, `internal/scan/pickle/pickle_test.go` or `cmd/hangar/unsafe_bypass_test.go`:

- joblib array bytes that hid an `os.system` call (`TestScanJoblibFindsCodeHiddenByArrayData`).
- A `.bin` skipped because an `.xml` of the same name existed (`TestScanFileReadsLoadableFilesByContent`).
- Error results that downloaded although Python's unpickler reads further (`TestGateRefusesPartialScans`, `TestScanReadsPastLongLines`).
- Compressed and archived names that were never scanned, such as `model.joblib.gz` and `checkpoint.pth.tar` (`TestKindOfCompressedAndArchivedNames`).
- Objects held under a harmless name at a branch tip but as a pickle in an older Commit (`TestGateChecksEveryCommitThatHoldsAnObject`).
- Pickles stored in git rather than LFS, served by `git clone` (`TestGateChecksGitFetches`).
- Protocol 0 escapes, Python 2 module names, dotted names and extension opcodes that hid imports (`TestScanDecodesProtocol0StringsAsPython`, `TestClassifyMapsPython2Names`, `TestClassifyDottedNames`, `TestScanTreatsExtensionCodesAsUnnamedImports`).

### Scanner limits

These are accepted, documented, or waiting for a decision:

- **Suspicious imports download at the default level.** At `scan.block_level: unsafe`, only imports on the deny list block, so a function that runs code and is on neither list is suspicious and downloads; the level is the default because model classes from `transformers` and `sklearn` are on neither list. Operators who want what `torch.load(weights_only=True)` refuses set the level to `suspicious` (decided by James on 2026-10-18; `TestSuspiciousDownloadsFollowTheBlockLevel`).
- **Files are chosen by name.** A pickle under a name no standard loader uses is neither scanned nor gated. Code loaded with `trust_remote_code` can load anything, but it already runs the Repo's Python.
- **Some formats give partial results** and are refused when blocking is on: xz and lzma compression (Go's standard library has no decoder), old PyTorch tar archives, unusual joblib arrays, and zip archives Go cannot open.
- **joblib wrapper aliases.** The scanner recognises `NumpyArrayWrapper` by name; a module that imports it under another name is not recognised, although its import still shows.
- **Long histories.** Objects of Repos with more than 20,000 Commits or 50,000 LFS file changes are decided from the history the gate can walk, and git fetches of such Repos are refused.
- **`redirect` mode** presigned URLs are checked when issued, not when used.

## LFS object references

Decided by James on 2026-10-18 ("check at Commit"); the rules are in [spec 04, "Who may reference an LFS object"](https://github.com/hangar-hub/hangar/blob/main/docs/spec/04-git.md#who-may-reference-an-lfs-object). What remains:

- **Pulls into a Mirror are not checked.** A Pull marks an object present when the Upstream's Repo names it and the Instance already stores it, without downloading it again, and Pull-through serves such an object from the store. A user who can start a Pull from an Upstream on which they can commit a pointer without its bytes could reach an object they cannot read. Instance admins choose every Upstream; huggingface.co, and Hangar from this version, refuse such commits.
- **Xet proof rests on chunk structure.** A shard proves the upload when its file rebuilds, from xorbs Hangar holds, into the claimed SHA-256. A client that knows the xorb hashes and chunk ranges of someone else's file could name them without sending the bytes. Hangar never serves Xet reconstruction data, file hashes or xorb hashes (it offers no Xet downloads), so they are known only to whoever had the file.
- **Uploaders are recorded for good.** A User who once uploaded an object may reference it for as long as it is stored, also after losing access to the Repo it came from; they held the bytes. Garbage collection removes the record with the object.
- **Objects stored before the change** have no uploaders, so only readers of the Repos holding them may reference them without an upload.
- **Readable means `read_files` now.** A User who can read a Repo that holds an object may reference it in their own Repos, including a public one, which makes it readable to everyone. That is the same as downloading and uploading it again.

## Token scopes

The scopes follow the decision of 2026-10-17 that a write Token administers Repos through the Hub API only, as on huggingface.co, and nowhere else. A Token has one scope, applied on top of the user's own Role:

| Scope | Allows |
|---|---|
| `read` | Read files, metadata and history the user can read. |
| `write` | Also push, commit, upload and start Pulls. On Repos where the user's Role is `admin`, also the `hub_admin` action: delete a Repo, change its visibility or gating, and rename it within its Namespace, only through the Hub API endpoints `huggingface_hub` calls (`DELETE /api/repos/delete`, `PUT /api/{type}s/{repo}/settings`, and `POST /api/repos/move` when the Namespace does not change). |
| `admin` | Everything else: transfers to another Namespace, webhooks, collaborators, access requests, every native API admin action, Organisation Members and, for Instance admins, the Instance. |

Restrictions to Namespaces or Repos narrow a Token and never widen it. Instance administration needs an unrestricted `admin` Token (`internal/auth/authz.go`). Tests: `TestPermissionMatrix` (with its `hub_admin` column), `TestWriteTokenAdministersReposThroughHubAPIOnly`, and the compat test `test_write_token_administers_own_repos`. See also [Permissions](../concepts/permissions.md).

## CSRF and CSP

**Cross-site request forgery.** Three layers protect state-changing requests:

1. The session cookie is `SameSite=Lax` and `HttpOnly`.
2. Every state-changing native API call from the UI carries the session's CSRF token in `X-CSRF-Token` (double submit).
3. A request with a method other than `GET`, `HEAD` or `OPTIONS` that carries the session cookie or HTTP Basic credentials is refused with 403 when `Sec-Fetch-Site` is present and is not `same-origin` or `none`, or, without it, when `Origin` names neither the request's host nor `server.public_url`. This uses Go's `net/http.CrossOriginProtection` (`internal/auth/crossorigin.go`). Clients that send neither header, such as git, git-lfs, `huggingface_hub`, the CLI and Ollama, are unaffected.

Bearer Tokens are exempt, because a browser never adds them by itself. Tests: `TestAuthenticateSessionsAndCSRF`, `TestCrossOriginBrowserCredentials`, `TestBasicCredentialsOnlyForGit`. A browser client on a sibling subdomain is refused (`Sec-Fetch-Site: same-site`); Hangar serves its UI and APIs from one origin.

**Content Security Policy.** UI pages (`internal/web/web.go`) send:

```text
default-src 'self'; script-src 'self'; style-src 'self' 'nonce-<per response>';
img-src 'self' data: blob: https:; media-src 'self' blob:; font-src 'self';
connect-src 'self' <configured>; worker-src 'self' blob:; object-src 'none';
base-uri 'self'; form-action 'self'; frame-ancestors 'none'
```

with `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `Cross-Origin-Opener-Policy: same-origin` and a `Permissions-Policy` that turns off camera, microphone, geolocation, payment, USB, serial, HID and Bluetooth. No inline scripts are allowed. Tests: `TestServesIndexForUIRoutesWithCSP`, `TestCSPWithoutNonce`.

File downloads are attachments with `Content-Type: application/octet-stream` and `nosniff` (`internal/blob/download.go`); Dataset Viewer assets add `Content-Security-Policy: default-src 'none'; sandbox` (`internal/datasets/viewer/assets.go`). `img-src https:` lets cards show external images unless `ui.external_images` is off.

## SSRF policy

Hangar makes outbound requests to Upstreams and webhook receivers. Both use `httpx.IsReservedAddr` and `httpx.IsPrivateAddr` (`internal/httpx/netaddr.go`), checked in the dialler on the address actually dialled, after DNS resolution, so DNS rebinding cannot get past them.

| Destination | Upstreams | Webhooks |
|---|---|---|
| Reserved: link-local (including 169.254.169.254), other metadata services (100.100.100.200, `fd00:ec2::254`), unspecified (`0.0.0.0`), "this network", broadcast, multicast, and NAT64 addresses that embed one | Never | Never |
| Private: loopback, RFC 1918, carrier-grade NAT, unique local | The Upstream's own host, wherever it resolves; a redirect to another host only when the Upstream's own host is private | Only when `webhooks.allow_private_targets` is on (off by default) |
| Public | Yes | Yes |
| Redirects | Followed (at most 10), credentials dropped on any change of scheme, host or port | Not followed |

A refused connection is not retried, and its error names the address and the reason, never the URL. Tests: `TestUpstreamRedirectsCannotReachTheLocalNetwork`, `TestUpstreamNeverReachesReservedAddresses`, `TestCheckAddr`, `TestReservedTargetsAreAlwaysRefused`, `TestTestDeliveryAndPrivateTargets`. See also [Mirrors and Upstreams](../concepts/mirrors.md#upstreams) and [Webhooks](../guides/webhooks.md).

Not covered by the policy:

- OIDC discovery and token requests go to the provider URLs an Instance admin configures.
- IPv6 transition addresses such as 6to4 and Teredo are not classed as private, because they route through relays on the internet.
- A proxy from `HTTP_PROXY` or `HTTPS_PROXY` decides where Upstream requests go; Hangar checks only the proxy's address.

## Supply chain

| Control | Where | Notes |
|---|---|---|
| Known vulnerabilities in Go code | `govulncheck` in `ci.yml` | At the phase 3 review, no vulnerabilities in called code or imported packages, after `kin-openapi` was upgraded to v0.144.0. `pnpm audit --prod` and `pip-audit` (test and docs dependencies only) found none. |
| Static analysis | `golangci-lint` with `gosec` in `ci.yml`; CodeQL for Go and TypeScript in `codeql.yml` | CodeQL runs on pull requests, on `main` and weekly. |
| Fuzzing | `fuzz.yml`, nightly, every `Fuzz*` target through `scripts/fuzz.sh` | 34 targets, including the parsers of untrusted input listed in [The worker](#9-the-worker). |
| Dependency updates | `.github/dependabot.yml` | Go modules, npm, GitHub Actions, Docker, Compose and uv, weekly. |
| Signed images | `release.yml` | cosign keyless signing through GitHub OIDC, with an SPDX SBOM attestation (syft) and SLSA build provenance. The Helm chart is signed too. |
| Signed archives and SBOMs | `.goreleaser.yaml` | `checksums.txt` signed into a Sigstore bundle, and an SPDX SBOM per archive. `TestGoReleaserSignsWithACosign3Bundle` (`deploy/release_test.go`). |
| Image scanning | Trivy in `release.yml` | The release fails on critical CVEs that have a fix. |
| Minimal image | `Dockerfile` | `scratch` with a Wolfi root filesystem, non-root, no shell. |

How to verify signatures is in [Verify the image](../operate/security.md#verify-the-image).

**Residual risks:**

- Workflows pin every GitHub Action to a commit SHA, and `scripts/check-action-refs.sh` (run by `make lint-deploy`) checks that each SHA is the commit of the release named in its comment, so a moved tag changes nothing. What remains is trust in each pinned release and in the Dependabot pull requests that move the pins, which a maintainer reviews.
- Base images are named by tag (`golang:<version>-alpine`, `node:<version>-alpine`, `cgr.dev/chainguard/wolfi-base:latest`), not by digest.
- Keyless signing, the transparency log and the multi-arch push have only been rehearsed offline with a throwaway key (`make release-rehearsal`); they run for real at the first release.

## Open decisions

None. James decided the three Phase 3 security questions on 2026-10-18; each decision and its tests are recorded in `docs/build/decisions.md` and in the rows above.

## Report a vulnerability

Report security problems privately through GitHub's private vulnerability reporting, as the [security policy](../project/security.md) (`SECURITY.md`) describes. Do not open a public issue, discussion or pull request. Security fixes go into the latest minor release.
