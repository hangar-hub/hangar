# 04. git storage, smart HTTP and git-lfs

The research behind this page, with tests against real `git`, git-lfs and huggingface.co, is [`docs/research/git-server-research.md`](../research/git-server-research.md). It is normative for W2 and W3 where this page does not say otherwise.

## Decisions

| Concern | Decision |
|---|---|
| Object storage | Immutable packfiles (`.pack`, `.idx`, `.rev`) in the Blob store under `git/<repo-uuid>/pack/pack-<sha1>.*`. Never loose objects. |
| Source of truth | The `refs` and `packs` tables plus the pack bytes. A ref may only point at a Commit whose closure is in `live` packs. |
| Fetch and clone | The real `git` binary: `git upload-pack --stateless-rpc [--advertise-refs]`, protocol v0 and v2, shallow, `filter`, bitmaps. |
| Push | go-git v6 `transport.ReceivePack` (pinned to an exact v6 pre-release or commit, wrapped behind `internal/gitstore` interfaces), with an in-memory ref snapshot and a `PreReceive` hook that makes the push atomic. |
| Server-side Commits (Hub API, UI, Pulls of local Repos) | The git binary in a temporary staging repository whose alternates point at the pack cache (`hash-object`, `update-index` on a temporary index, `write-tree`, `commit-tree`), then `pack-objects` of only the new objects and the same upload-and-CAS path as a push. Objects are byte-identical to git's by construction (decided in phase 2, `docs/build/requests/w2.md` item 1). |
| Mirrors | Fetch upstream packs with go-git v6 (client) and store them verbatim, so Commit SHAs are identical to the Upstream. |
| Replicas | Stateless apart from a disposable local pack cache (`server.git_cache_dir`, default `/var/cache/hangar/git`, LRU-bounded by `server.git_cache_size`, default 10 GiB). |
| git-lfs | Batch API with `basic` transfer and Hugging Face's `multipart` custom transfer; `verify`; `/locks*` return 404. |
| Image | Needs `git` (2.39 or later). The final image is Chainguard `wolfi-base` with `git` installed, running as non-root. This replaces distroless in [12](12-packaging-and-release.md). |

## Data model additions

These replace or extend the tables in [02](02-architecture.md):

- `refs(repo_id, name, target, updated_at)`, primary key `(repo_id, name)`. Compare-and-swap is on `target` (see "Ref updates"), so there is no `version` column.
- `packs(repo_id, pack_sha1, size, object_count, state, created_at, retired_at)`, where `state` is `pending`, `live`, `retired`, `orphaned` or `deleting`.
- `repos.head_ref` (default `refs/heads/main`).

## Reading a Repo on a replica

For every git request, and for any `repo.Service` read that needs git objects:

1. Read the live pack list: `SELECT pack_sha1 FROM packs WHERE repo_id = ? AND state = 'live'`.
2. Download any missing packs into the cache. Packs are immutable and never need invalidating. Use singleflight per Repo, so concurrent requests share one download.
3. Build a temporary **view** directory with `HEAD` (`ref: <head_ref>`), a minimal `config`, `objects/info/alternates` pointing at the cache, and `packed-refs` written from a single snapshot of the `refs` table.
4. Remove the view when the request ends.

`repo.Service` reads (trees, blobs, Commit logs) use go-git on the same view, or the `tree_entries` and `commits` caches in the database where they exist. The caches are filled when a ref moves, so the Hub API's hot paths (`model_info`, `tree`, `resolve`) do not touch git objects.

## Fetch and clone

- `GET /{repo}[.git]/info/refs?service=git-upload-pack`, then `POST /{repo}[.git]/git-upload-pack`, for models, plus the same under `/datasets/`.
- Run `git -c uploadpack.allowFilter=true -c uploadpack.allowAnySHA1InWant=false -c uploadpack.allowReachableSHA1InWant=true upload-pack --stateless-rpc [--advertise-refs] <view>`, with `GIT_PROTOCOL` taken from the `Git-Protocol` request header. Stream stdin and stdout; never buffer whole packs. Handle `Content-Encoding: gzip` request bodies.
- Protocol v2 sends `ls-refs` and `fetch` as separate POSTs that may land on different replicas. This is safe because every advertised SHA is in live packs.
- Run the subprocess with a minimal environment (`GIT_CONFIG_NOSYSTEM=1`, `HOME` set to an empty temp dir, no user config), a CPU and wall-clock limit (`server.git_timeout`, default 1 hour), and kill it when the client disconnects.
- Read access follows [05](05-auth.md); private Repos return 404 to callers without read access.

## Push

- `GET /{repo}[.git]/info/refs?service=git-receive-pack` without credentials returns `401` with `WWW-Authenticate: Basic realm="Hangar"`, so git asks the credential helper. With credentials and no `write` access: 403 for Repos the caller can read, 404 otherwise.
- `POST /{repo}[.git]/git-receive-pack` runs go-git `ReceivePack` on a filesystem storer over the view, with alternates to the cache and refs from an in-memory snapshot.
- go-git and `git index-pack` rebuild every delta in memory, so the request body passes through `gitstore.LimitReceivePack` first, which reads the pack as it streams and holds back its trailer until every object passes ([Pack and tree limits](#pack-and-tree-limits)).
- The `PreReceive` hook, in order:
  1. **Policy:**
     - the token has `write` scope and passes its restrictions
     - the Repo is not a Mirror (unless it was detached)
     - ref names are valid and live under `refs/heads/` or `refs/tags/`; `refs/pr/*` is writable only through the pull request flow
     - force-push and delete rules: the default branch cannot be deleted, and force-pushing it is allowed only for Namespace admins unless the setting `git.allow_force_push` is true
     - non-LFS blobs are under `git.max_blob_size` (default 10 MiB, as on the Hub), with an error message that explains how to track the file with LFS
     - the pack is under `git.max_pack_size` (default 2 GiB) and within the [pack and tree limits](#pack-and-tree-limits)
     - the push fits the Namespace quota
     - every LFS pointer in the new Commits refers to an LFS object that exists and is verified
  2. Insert `packs(state = 'pending')`, then upload `.pack`, `.idx` and `.rev` to the Blob store. A pack already recorded with the same SHA-1, such as one a failed earlier attempt of the same push left `orphaned`, becomes `pending` again; if a sweep is deleting it, wait until the sweep has finished.
  3. In one transaction, run a compare-and-swap for every ref command (sorted by name, to avoid deadlocks), set the pack `live` (failing if it is no longer `pending` or `live`), update `repo_lfs` reachability, and enqueue cache fills, `index`, `scan` and webhook Jobs.
  4. On any compare-and-swap failure, roll back, mark the pack `orphaned`, and reject every ref with `reference has changed concurrently, fetch first`. The push is all or nothing.
- go-git does not advertise `atomic`, so `git push --atomic` fails on the client even though every push is atomic. Document it, and contribute the capability upstream if time allows.

### Ref updates

The single function `gitstore.UpdateRefs(ctx, repoID, []RefUpdate{Name, Old, New}) error` is used by pushes, Hub API Commits, UI Commits, PR merges and Mirror syncs:

```sql
-- update: UPDATE refs SET target=$new, updated_at=now() WHERE repo_id=$r AND name=$ref AND target=$old   -- rowcount must be 1
-- create: INSERT INTO refs(repo_id,name,target) VALUES ($r,$ref,$new) ON CONFLICT DO NOTHING             -- rowcount must be 1
-- delete: DELETE FROM refs WHERE repo_id=$r AND name=$ref AND target=$old                                 -- rowcount must be 1
```

`READ COMMITTED` is enough on Postgres. On SQLite, the single process serialises writes.

## Server-side Commits

`repo.Service.Commit`:

1. Resolve the parent: `parent_commit` if given (Hub API `parentCommit`), else the branch tip.
2. Build blobs (pointer files for LFS paths), trees and the Commit with the git binary in a staging repository. LFS pointer files must be byte-exact with what `git lfs pointer` produces (see the research doc, section 3.6). Author and committer come from the user's display name and email, and the time is the server time in UTC.
3. Decide LFS or regular per path using the Repo's `.gitattributes` at the parent Commit, falling back to the Hub's default patterns.
4. Encode a pack of the new objects only, then follow push steps 2 to 4 with `Old = parent`.
5. A compare-and-swap conflict when the client sent `parent_commit` returns the Hub's 412 error at once, with no retry. Without `parent_commit`, retry up to 7 times on the new tip if the change still applies cleanly (no path in the Commit was changed in between), else 409. Before each retry, wait a random time between 0 and `min(2 s, base × 2^(n-1))` for retry `n`, where `base` is the longer of 20 ms and the time the failed attempt took, so Commits that collided retry apart instead of colliding again ("full jitter" exponential backoff). Decided by James on 2026-10-18; see [decisions](../build/decisions.md).

New Repos get an initial Commit on `main` with `.gitattributes` (the Hub's default LFS patterns, copied in `internal/repo/gitattributes_default.txt`) and, for UI-created Repos, a starter `README.md`.

## Mirrors

- Plan step ([06](06-upstreams-and-pulls.md)): fetch from `<upstream>/<repo>` (or `/datasets/<repo>`) with go-git v6 as a client, with these refspecs: `refs/heads/*`, `refs/tags/*`, `refs/pr/*` and `refs/convert/*`. Fetch full blobs, not `blob:none`, because Hangar must serve complete packs. Later syncs fetch with `have`s, so only new objects come down.
- Store each received pack verbatim, then update refs with the same compare-and-swap. Upstream force-pushes are applied, keeping a `refs/hangar/previous/<timestamp>` ref for any tip that disappears.
- LFS pointers in the fetched history become `repo_lfs` rows with `present = false`, unless the object is already stored or selected.
- Fetched packs and their Commits pass the same [pack and tree limits](#pack-and-tree-limits) as pushes, except the limits on objects that are not deltas and on the number of objects, which Mirrors take from the Upstream as they are. Hangar reads at most 100,000 refs from an Upstream's advertisement.

## Pack and tree limits

Decided in Phase 3 (see [decisions](../build/decisions.md), "Git input limits"). Every pushed or fetched pack, and every Commit in it, must stay within these limits, or the push or Pull is refused with a message that names the limit:

| Limit | Value | Why |
|---|---|---|
| Unpacked size of one object, and of a delta's own instructions | the larger of `git.max_blob_size` and 64 MiB (pushes only) | Saves unpacking a compressed object that the blob policy would refuse anyway. |
| Base and result size a delta declares | the larger of `git.max_blob_size` and 64 MiB | go-git and `git index-pack` hold both in memory: a 16 KiB delta can copy a 16 MiB base 64 times into 1 GiB. |
| Objects in one pack | 2,000,000 (pushes only) | Both keep a record of every object while indexing. |
| Unpacked size of the whole pack | four times `git.max_pack_size`, and at least 1 GiB | Bounds the work of unpacking. |
| Ref updates in one push | 10,000 | Each is a compare-and-swap in one transaction. |
| Files and folders in one Commit | 1,000,000 (`repo.Options.MaxTreePaths`) | git stores a folder once however often a tree names it, so a few hundred bytes can describe billions of paths, which the tree cache, indexing and zip export would list. |
| Length of a path | 4,096 bytes (`model.MaxPathLength`) | Also bounds how deep folders nest. |
| Names | every name `model.ValidPathComponent` accepts: not empty, `.` or `..`; no `/`, `\` or NUL; and nothing a client could check out as `.git` (any case, `git~1`, trailing dots or spaces, HFS+ ignorable code points) | Names like these escape a download folder or write into a client's `.git` folder (CVE-2014-9390). git's fsck refuses most of them on push; Hangar refuses them on Pulls too. |

The tree check reads each tree once however often it is named, so it costs what the distinct trees cost, not what they expand to. The tree cache stops listing at the same limits, so a Commit stored before them fails to list instead of filling memory, and zip export refuses a path that would extract outside its folder.

## git-lfs

Endpoints under `/{repo}.git/info/lfs/` (and `/datasets/...`). The server tells clients where to find it through the standard `<clone URL>.git/info/lfs` discovery rule.

### Batch

`POST .../objects/batch` (`application/vnd.git-lfs+json`):

- **Download:**
  - Present objects: a presigned S3 `GET` (in `redirect` mode) or a Hangar URL with a signed token, `authenticated: true`, and `expires_in` set.
  - Objects not present in a Mirror: if Pull-through is enabled and the caller may use it, return a Hangar URL that starts the transfer and streams bytes as they arrive (the same mechanism as `resolve`). Otherwise return a per-object `error: {code: 404, message: "Not in arsenal. Pull this file with: hangar pull <repo> --include <path>, or clone with GIT_LFS_SKIP_SMUDGE=1"}`.
- **Upload:** objects that already exist and are verified, and that the caller may reference (see [Who may reference an LFS object](#who-may-reference-an-lfs-object)), get no `actions` (deduplication). Every other object gets upload actions, including one that is stored but that the caller did not upload and cannot read, so the answer never says whether such an object exists or how large it is; a size mismatch is reported with the stored size only for objects the caller may reference. Otherwise:
  - The client offered `multipart` (Hugging Face's custom transfer, used by `hf lfs-enable-largefiles`) and the object is over the multipart threshold (default 64 MiB): Hangar creates a multipart upload, `actions.upload.href` is the completion URL, and `actions.upload.header` has `chunk_size` and zero-padded part numbers mapping to presigned part URLs (or Hangar part URLs with a signed token on the filesystem backend).
  - `basic` and at most 5 GiB on S3: a presigned `PUT`. Sign `x-amz-checksum-sha256` into it when the store supports it (W1 tests this against AWS and MinIO, and records the result in `docs/research/`).
  - `basic` and over 5 GiB, or the filesystem backend: a Hangar upload URL with a signed token. Hangar streams the body into an S3 multipart upload (part size `max(16 MiB, ceil(size/10000))`), or a temp file and rename, hashing as it goes. It completes only if SHA-256 and size match, so a corrupt object is never visible.
  - Every upload response includes a `verify` action.
- **Verify:** `POST` with `{oid, size}`. Checks size, and makes sure the SHA-256 has been verified, either by the signed checksum or by Hangar re-hashing the stored object (streaming). Marks the object verified. A Commit or push that references an object still being verified waits up to `lfs.verify_wait` (default 10 minutes) and then fails with a clear error. Verify succeeds only for an upload by the User the batch answered (the signed URL names them): for an object that is stored but that this User did not upload, it answers 404 as for a missing object.

### Who may reference an LFS object

Decided by James on 2026-10-18 ("check at Commit"). LFS objects are stored once per Instance (UP-6), but a Commit may add a pointer to an object only when its author:

- uploaded its bytes, through any path: the git-lfs basic or multipart transfer, a presigned `PUT` and its verify, the native API's upload plan, a Hub API `file` line that `.gitattributes` stores in LFS, or a Xet shard that rebuilds into it; or
- can read the files of a Repo that holds it (the `read_files` permission, which includes Token restrictions and gating), including the Repo the Commit goes to.

Hangar records each uploader in `lfs_uploaders(oid, user_id)`. An upload of bytes that are already stored is read and checked against the SHA-256 and size like a new one, records the uploader, and is then discarded, so no second copy is kept. Objects stored before the table existed have no uploaders and stay usable through the Repos that already hold them.

The rule applies to git pushes, Hub API commits (including `create_pr=1`, checked before the pull request opens), native API commits and Xet uploads, which commit through the Hub API. A pointer to any other object is refused with the same error as for an object that was never uploaded (`the push refers to LFS object <oid> (<size> bytes), which has not been uploaded` for pushes, `lfs object is missing or not verified` for commits), with the size the client sent, never the stored one. huggingface_hub and git-lfs ask the batch API first, get upload actions and send the bytes, so they succeed without any change. Hangar itself (Parquet export, pull request merges within a Repo, Pulls into a Mirror) is not checked.

Single-part `PUT`s, part `PUT`s and the multipart completion `POST` carry no `Authorization` from Hugging Face clients, so every one of these URLs must authorise itself (presigned or signed token, scoped to one object and one upload, expiring in `lfs.upload_url_ttl`, default 24 hours).

### Locks

Every `/locks*` endpoint returns `404` with `{"message": "Locking is not supported"}`. git-lfs then quietly disables lock verification for the remote, with no warning on every push.

### Auth

HTTP Basic: any username, token as password, the same as git. Unauthenticated batch requests for public Repos are allowed for downloads only. Uploads need `write`, or the right to open a pull request on the Repo: `huggingface_hub` uploads LFS files for `create_pr=True` through the batch API when Xet is off, and the batch request does not say that the bytes are for a pull request. Such a user can only use the objects in a Commit they may make, which is one that opens or updates a pull request, and pushing still needs `write`.

## Compaction and garbage collection

- `git.compact` Job, one leader at a time (Job lease): when a Repo has more than `git.compact_threshold` live packs (default 20), materialise a view, run `git repack -a -d --write-bitmap-index`, upload the new pack, and in one transaction mark it `live` and the old ones `retired`.
- Delete `retired` and `orphaned` packs after `git.pack_grace_period` (default 1 hour, longer than the longest request). A sweep first claims a pack by moving it to `deleting` with a conditional update that checks it is still eligible, then deletes its files, then its row. A sweep that stops part of the way through leaves the pack `deleting`, and a later sweep finishes it.
- Prune only objects unreachable from every ref, including `refs/pr/*` and `refs/hangar/*`. Never prune Mirrors.
- `hangar admin gc --git` runs it on demand.

## Limits and risks to watch

- go-git v6 is pre-release: pin it, keep it behind interfaces, and run the `compat/git` suite on every PR. If go-git's receive-pack proves unreliable during the build, the fallback is `git receive-pack --stateless-rpc` with a pre-receive hook that calls back into Hangar (described in the research doc).
- go-git is not safe for concurrent use: one storer per request, no shared caches.
- The first request for a Repo on a replica downloads its packs. Expose cache hit and miss metrics (`hangar.git.cache.*`).
- SQLite is single-replica only; the filesystem backend with several replicas needs a shared read-write-many volume with atomic rename (tested in CI on NFS).
