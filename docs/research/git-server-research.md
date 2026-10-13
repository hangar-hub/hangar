# Hangar: git smart HTTP + git-lfs on stateless replicas (implementer's reference)

Research date: 2026-10-13. Scope: git storage, smart HTTP, LFS, mirroring, S3 multipart.
The HF HTTP API is out of scope (another agent covers it).

Evidence labels used below:
- **[src]**: read in the source at the commit/tag given.
- **[doc]**: official documentation.
- **[test]**: I ran it on this machine (git 2.54.0, git-lfs 3.7.1, Go 1.26.1, macOS arm64).
- **[unverified]**: inference or memory. Check before you depend on it.

Pinned sources:
- go-git `main` @ `bb92758a` (2026-10-09), which is after `v6.0.0-beta.1`. Also `v5.19.3`.
- git-lfs `main` @ `0043a645` (2026-09-22). Latest release is v3.8.0 (2026-08-28).
- minio-go `master` (2026-10-12).
- aws-sdk-go-v2 `main`.
- huggingface_hub `main`.

---

## TL;DR

1. **go-git v5 cannot back a real server.** go-git v6 can. v6 has a full `backend` package, its own smart-HTTP handler and protocol v2 upload-pack, and it is in beta: `v6.0.0-beta.1` came out on 2026-10-04.
   - In my tests real `git` clients could clone (v0 and v2), shallow-clone over v2, and push. A racing push was correctly rejected by compare-and-swap (CAS).
   - The tests also found problems. Shallow clone over v0 stateless HTTP is broken (open issue #2011). Partial-clone filters are not served. Upload-pack is about 200x slower than `git upload-pack`, because it re-deltifies and re-compresses every object: 12 s of CPU per full clone of a 15 MiB repo.
2. **I found no major forge that serves git with go-git.** Gitea, Soft Serve and Hugging Face itself all run the real `git` binary. HF advertises `agent=git/2.53.0-rc0`.
3. **Recommendation: a hybrid design.**
   - Store **immutable packfiles** in S3 or the filesystem, keyed by repo. Keep **refs and the pack list in the DB**.
   - Each replica keeps a **disposable local pack cache**. For each request it builds a throwaway "view" bare repo: alternates point at the cache and `packed-refs` comes from a DB snapshot.
   - Serve **fetch/clone with `git upload-pack --stateless-rpc`**.
   - Handle **push with go-git v6 `transport.ReceivePack`** in process. A `PreReceive` hook uploads the pack and then runs a **DB transaction that CASes the refs**.
   - Use the go-git encoder for packs created server-side (HF commit API) and for mirror ingest.
4. **LFS is straightforward.**
   - Use the Batch API plus `basic` transfer with presigned S3 GET/PUT URLs and `authenticated: true`.
   - Return **404 from `/locks/verify`**. git-lfs then quietly writes `lfs.<url>.locksverify=false` and prints no warning (verified in source).
   - Presigned `basic` PUT tops out at 5 GiB on AWS. Above that, either proxy the upload through Hangar (stream it into S3 multipart) or offer HF's `multipart` custom transfer.
5. **Mirroring is cheap and keeps SHAs.**
   - `GIT_LFS_SKIP_SMUDGE=1 git clone` of `jinaai/jina-embeddings-v3` pulls a 16 MiB `.git` (67 commits, 571 objects) against 7+ GB of LFS content, and HEAD equals the HF API `sha`.
   - The go-git v6 client produced the same HEAD.
   - HF serves `--filter=blob:none` (180 KiB) and `--depth 1`.
   - Pointer oids match `lfs.oid` in the tree API and `X-Linked-Etag` on resolve.

---

## 1. go-git

### 1.1 Version status

- v5 is stable: v5.19.3 came out on 2026-10-04. v6 is pre-release: `v6.0.0-beta.1` also came out on 2026-10-04, after alpha.1 to alpha.5 between May and July 2026. Both lines are released in lockstep [src: GitHub releases].
- The v6 module path is `github.com/go-git/go-git/v6`. v6 breaks the API of v5.
- **Pin a specific commit or the beta tag, and plan for API churn before GA.**

### 1.2 v5 server (`plumbing/transport/server`, v5.19.3): not usable

[src: `plumbing/transport/server/server.go`]
- The upload-pack session advertises only `agent` and `ofs-delta` (`setSupportedCapabilities`, lines 191-205). That means no `side-band`, `multi_ack` or `shallow`.
- `if len(req.Shallows) > 0 { return nil, fmt.Errorf("shallow not supported") }` (line 161).
- The pack window is hard-coded: `e.Encode(objs, 10)` with `// TODO: plumb through a pack window`.
- `//TODO: Implement 'atomic' update of references.` (line 252).
- v5 has no HTTP server handler at all. You would have to write the smart-HTTP framing yourself.

### 1.3 v6 server

v6 replaced the old `server` package with `plumbing/transport/{upload_pack,receive_pack,serve}.go` and a new top-level **`backend`** package. `backend` provides `Backend.ServeHTTP` (smart HTTP) and `Backend.Serve` (stream transports) [src: `backend/backend.go`, `backend/http.go`].

**Integration points:**
- `transport.Loader` resolves a URL to a `storage.Storer`.
- `transport.UploadPack(ctx, st, r, w, &UploadPackRequest{GitProtocol, AdvertiseRefs, StatelessRPC, SkipDeltaCompression})`.
- `transport.ReceivePack(ctx, st, r, w, &ReceivePackRequest{..., Hooks: ReceivePackHooks{PreReceive, PostReceive}})`.
- You can skip `backend.ServeHTTP` and call these functions directly from your own router. That is the better choice: you own the auth and routing. Note that `ServeHTTP` only checks that an `Authorization` header is *present* on receive-pack.

**What `backend/http.go` provides:**
- Routes for `GET <repo>/info/refs?service=` and `POST <repo>/git-upload-pack|git-receive-pack`.
- gzip request bodies, `Git-Protocol` header pass-through, and chunked flushing.
- A strict `Content-Type` check: 403 if it is not `application/x-git-<svc>-request`.
- Dumb-protocol paths answer 403.

**Capabilities** [src: `plumbing/transport/serve.go` `AdvertiseRefs` lines 60-80 and `serverV2Capabilities` line 157]:

| Area | v6 server behaviour |
|---|---|
| Protocol v0/v1 upload-pack | `multi_ack`, `multi_ack_detailed`, `side-band`, `side-band-64k`, `ofs-delta`, `no-progress`, `shallow`, `object-format`, `symref`. **Not advertised:** `thin-pack`, `include-tag` (TODO), `deepen-since` (TODO), `no-done`, `filter`, `allow-*-sha1-in-want`. |
| Protocol v2 upload-pack | `ls-refs`, `fetch=shallow` (handles deepen, deepen-since, deepen-not, deepen-relative), `object-format`. Comment lists TODOs: `ls-refs=unborn`, `fetch=filter`, `ref-in-want`, `sideband-all`, `packfile-uris`, `wait-for-done`, `server-option`, `object-info`. |
| receive-pack | `report-status`, `delete-refs`, `push-options`, `quiet`, `ofs-delta`, `side-band-64k`, and **`no-thin`**, so clients send self-contained packs. **`atomic` is not advertised** (`// TODO: support atomic`, serve.go:69), so `git push --atomic` will be refused by the client. |
| Stale docs | `COMPATIBILITY.md` on `main` still says "pack-protocol v2 ❌". The code and my tests show v2 upload-pack works. For filter it says "Not offered when serving". |

**receive-pack flow** [src: `receive_pack.go`]:
1. `packfile.UpdateObjectStorage(st, rd)` at line 195. If the storer implements `storer.PackfileWriter`, the pack is streamed in as a pack. The filesystem storer writes `.pack`, `.idx` and `.rev`. Otherwise objects are parsed one at a time into `SetEncodedObject`.
2. Duplicate-ref and refname validation.
3. `Hooks.PreReceive` runs at line 291, after the unpack and before any ref changes. If it returns an error, every ref is refused with that message.
4. `updateReferences` runs:
   - update: `st.CheckAndSetReference(new, old)` at line 584, which is CAS.
   - **create: `st.SetReference`** at line 551, after a separate existence check. That is a TOCTOU window.
   - **delete: an old-value check, then a separate `RemoveReference`** at line 574.
   - Its doc comment says so directly: *"ReferenceStorer has no transaction covering these steps: callers must serialize concurrent writers if they require the entire operation to be atomic."*
5. `PostReceive` runs, then report-status is sent.

### 1.4 Empirical test: v6 `_examples/http-server` against the real git CLI [test]

Setup:
- Built from go-git `main@bb92758a`.
- Served bare copies of `hf-internal-testing/tiny-random-bert` (6 commits) and `jinaai/jina-embeddings-v3` (67 commits, 571 objects, 15 MiB pack).

| Test | Result |
|---|---|
| `git -c protocol.version=2 clone` | OK, HEAD `f171d7ba…` matches |
| `git -c protocol.version=0 clone` | OK |
| `git clone --depth 1` (v2, the default) | OK, 1 commit |
| `git -c protocol.version=0 clone --depth 1` | **FAIL**: `fatal: git fetch-pack: expected shallow list`. This is the known bug **go-git#2011** (open): the shallow line is missing in phase 2 of stateless HTTP. |
| `git clone --filter=blob:none` | Server ignores it: `warning: filtering not recognized by server, ignoring`, so a full clone is sent |
| `git push` (v0 and v2) | OK. Stored as `pack-*.pack/.idx/.rev`, and `git fsck` is clean |
| Push race: a client-B `pre-push` hook pushes client A after B has read the advertisement | B is rejected with `! [remote rejected] HEAD -> main (reference has changed concurrently)`. Server-side CAS works end to end. |
| Full push of jina (15 MiB) into an empty repo | 0.51 s |
| **Full clone of jina from the go-git server** | **12.1-12.5 s per clone** (3 runs, about 12 s CPU each, server RSS about 47 MB). For comparison, `git upload-pack --stateless-rpc` on the same repo takes **0.06 s**, and a `file://` clone with the git binary takes 0.30 s. |
| Same clone with `pack.window=0` in the repo config (no delta search) | 1.96 s, and the pack grows from 14 MiB to 17 MiB |

Cause of the slow clone [src: `upload_pack.go:297-308`, `plumbing/format/packfile/delta_selector.go`]:
- Every fetch runs `packfile.NewEncoder(...).Encode(objs, packWindow)` (default `pack.window` is 10), which decompresses every object and searches for deltas from scratch.
- Compressed data and existing deltas are not reused the way `git pack-objects` reuses them.
- go-git has no reachability bitmaps for serving.

### 1.5 Other limitations and bugs that matter for server use

- **#2011** (open): shallow over v0 stateless HTTP. Reproduced above.
- **#2519** (open, 2026-10-11): `serveUploadPackV2` errors on non-`command=` capability lines such as `object-format=sha1`, which breaks `go get` from a go-git server. Other clients may hit the same thing [unverified which ones].
- **#1628** (open): asks how to serve depth-limited packs from full repos. It is mostly answered by the v6 deepen code, but read it.
- **#773** (open): "go-git is not thread-safe… concurrent reads may lead to repositories becoming corrupted". **Use one Storer instance per request.** Never share `Repository` or storer caches across goroutines without locking.
- **#1601** (open): memory blow-up cloning linux. Irrelevant at model-repo scale, but it shows how go-git behaves at scale.
- No `thin-pack` on serve, which only makes incremental fetches larger. No `filter` on serve. No `atomic` push. `report-status-v2` is ❌ [src: COMPATIBILITY.md].
- The receive-pack create and delete paths are not CAS (see 1.3). A DB-backed storer has to close that gap. See §2.4.

### 1.6 Who uses go-git as a server?

- **Gitea / Forgejo** shell out. They run `gitcmd.NewCommand("receive-pack", "--stateless-rpc", "--advertise-refs", ".")` and add `--stateless-rpc` to upload-pack [src: `go-gitea/gitea@main routers/web/repo/githttp.go` lines 281, 389, 508].
- **Soft Serve** shells out with `exec.CommandContext(ctx, "git")` [src: `charmbracelet/soft-serve@main pkg/git/service.go:61`].
- **Hugging Face** runs real git behind a "git-frontend": its upload-pack advertisement says `agent=git/2.53.0-rc0.g83a69f1-Linux`, and receive-pack returns `401 www-authenticate: Basic realm="git-frontend"` [test].
- **Keybase** (`go/kbfs/kbfsgit`) uses go-git v5 with the filesystem storer over a custom billy FS (KBFS). That is the client and remote-helper side, not a smart-protocol server [src].
- I found **no production forge that serves smart HTTP with go-git**. The v6 `backend` package is new, and issue #2011 was filed by someone running a self-hosted marketplace server on it. [unverified: GitHub code search returned almost nothing; absence is not proof.]

### 1.7 Custom `storage.Storer` backed by S3 or SQL

- **Interfaces** [src: `plumbing/storer/*.go`, `storage/storer.go`]:
  - `storage.Storer` = `EncodedObjectStorer + ReferenceStorer (incl. CheckAndSetReference) + ShallowStorer + IndexStorer + ConfigStorer + ModuleStorer`.
  - Optional interfaces: `PackfileWriter` (receive a pack as a stream), `PromisorPackfileWriter`, `DeltaObjectStorer` (return deltas unresolved), `Transactioner`, `LooseObjectStorer`.
- **Examples:**
  - The go-git `_examples/storage` README points to an Aerospike-backed storer (`mcuadros/go-git-aerospike`, old src-d era).
  - `iainjreid/go-git-sql` (database/sql) and `muandane/go-gitgres` (Postgres, published 2026-03) exist on pkg.go.dev [unverified: maturity, licence, v6 compatibility]. Sources: [go-git-sql](https://pkg.go.dev/github.com/iainjreid/go-git-sql), [go-gitgres](https://pkg.go.dev/github.com/muandane/go-gitgres).
  - I found no S3-backed go-git storer.
- **Performance of an object-per-row or object-per-key design** [src plus inference]:
  - The encoder walks `revlist.Objects` and then calls `EncodedObject` once per object. Against S3 that is one GET per object: 571 GETs for jina, per clone.
  - The receive side, without `PackfileWriter`, calls `SetEncodedObject` once per object.
  - Loose storage also loses on-disk deltas. The 67 README versions in jina total about 49 MB undeltified, against a 15 MiB pack. You would still pay the delta search shown in 1.4 on every clone.
  - **Do not store loose objects in S3. Store packs.**

---

## 2. Alternatives and recommendation

### 2.1 Options

| Option | Notes |
|---|---|
| **`git` binary**: `git upload-pack --stateless-rpc [--advertise-refs]` (the Gitea pattern) or `git http-backend` CGI | Full protocol: v2, `filter`, shallow, thin packs, bitmaps, `allow-reachable-sha1-in-want`. Delta and compressed-data reuse gives the 0.06 s above. Needs a **local bare repo**. Costs a fork/exec per request and a `git` binary in the image. Pass `GIT_PROTOCOL` from the `Git-Protocol` header for v2. Prefer direct `upload-pack` over the CGI so you own the HTTP layer. |
| **git binary on a local cache synced from object storage** | Works with stateless replicas if the local repo is a **disposable cache of immutable packs**. Validated [test]: a per-request "view" dir holding `HEAD`, `config`, `packed-refs` (from the DB) and `objects/info/alternates` pointing at a shared pack-cache dir clones fine in 0.30 s, and serves `--filter=blob:none` with `uploadpack.allowFilter=true`. |
| **Gitaly-style** (stateful git RPC nodes) | Gitaly keeps repos on node-local disk: "Each Gitaly node must use its own dedicated storage disk"; NFS and cloud filesystems "are not supported"; Praefect replicates across nodes [doc: docs.gitlab.com/administration/gitaly]. That contradicts the stateless-replica requirement and is a lot of machinery. Reject for v1. |
| **libgit2 / git2go** | git2go is not archived but is dormant: last release v34.0.0 on 2022-10-06 for libgit2 1.5, last push 2024-03. libgit2 itself is at v1.9.7 (2026-08-13). It needs cgo. libgit2 `src/libgit2/transports/` holds only client transports (`smart_protocol.c`, `http.c`, `ssh*.c`, `local.c`), and I found **no server-side upload-pack or receive-pack** [src: directory listing]. It does have a pluggable ODB/refdb and `git_packbuilder`. Reject. |
| **Go pack libraries** | go-git `plumbing/format/packfile` (Parser, Encoder, idxfile, revfile) is the only complete pure-Go pack encode/decode, and you can use it standalone. `git-lfs/gitobj` "reads and writes loose and packed Git objects" but has no protocol or packing for serve [src: README]. |
| **go-git v6 for everything** | Pure Go, one static binary, no subprocess, in-process hooks. Costs: beta API, the bugs in 1.5, about 12 s CPU per full clone of a jina-sized repo (2 s with `pack.window=0`), no filter. Workable for an MVP if you also cache generated full-clone packs. |

### 2.2 Recommendation (architecture)

**Storage model (source of truth = DB + object store):**

```
DB (Postgres; SQLite only for single-replica mode, see Risks)
  repos(id, kind{model,dataset,space}, namespace, name, head_symref='refs/heads/main', mirror_of NULL|url, ...)
  refs(repo_id, name, target_sha1, updated_at, PRIMARY KEY(repo_id,name))
  packs(repo_id, pack_sha1, size, object_count, state{pending,live,retired}, created_at, retired_at)
  lfs_objects(oid_sha256 PK, size, state{present,absent,uploading}, storage_key, created_at)
  repo_lfs(repo_id, oid_sha256)               -- reachability for GC/quota; "absent" allowed for mirrors
Object store (S3 or FS, same key layout)
  git/<repo_id>/pack/pack-<sha1>.pack | .idx | .rev    (immutable, content-named)
  lfs/sha256/<aa>/<bb>/<oid>                            (immutable, content-addressed)
```

**Invariant:** a ref can only point to a commit whose closure is in `live` packs. A pack row becomes `live` in the **same DB transaction** that CASes the refs, and the pack bytes must already be in the object store before that transaction. Any SHA a client sees is therefore fetchable from any replica.

**Per-replica cache (disposable, for example an emptyDir):**
- Layout: `cache/<repo_id>/objects/pack/*`.
- On every git request:
  1. `SELECT pack_sha1 FROM packs WHERE repo_id=$1 AND state='live'`.
  2. Download any missing packs. They are immutable, so they never need invalidating. Use a singleflight per repo.
  3. Build a tmp **view** dir (cheap, about 5 small files): `HEAD` (`ref: <head_symref>`), `config`, `objects/info/alternates -> cache/<repo_id>/objects`, and `packed-refs` written from **one** `SELECT * FROM refs WHERE repo_id=$1` snapshot.

**Fetch/clone (`GET info/refs?service=git-upload-pack`, `POST git-upload-pack`):**
- Run `git -c uploadpack.allowFilter=true -c uploadpack.allowAnySHA1InWant=false upload-pack --stateless-rpc [--advertise-refs] <view>` with `GIT_PROTOCOL=<Git-Protocol header>`.
- With protocol v2, `ls-refs` and `fetch` are separate POSTs that may land on different replicas. The invariant makes that safe.

**Push (`info/refs?service=git-receive-pack`, `POST git-receive-pack`):**
- Use go-git v6 `transport.ReceivePack` on a `filesystem.Storage` opened on the view dir. Set `AlternatesFS` so existing objects resolve through the cache; go-git supports alternates [src: `storage/filesystem/storage.go:60`].
- The new pack lands in the view's `objects/pack` through `PackfileWriter`.
- **Ref layer:** wrap the storer so refs are an **in-memory snapshot** loaded at request start. go-git mutates the snapshot only.
- **`PreReceive` hook:**
  1. Policy: auth scope, protected refs, size limits, and "all LFS pointer blobs in new commits exist in `lfs_objects`". Mirrors may allow `absent`.
  2. Upload the new `.pack`, `.idx` and `.rev` to the object store and insert `packs(state='pending')`.
  3. Run one DB transaction with **CAS for every command** (below), set the pack row to `live`, and commit.
  4. On any CAS failure, roll back and return an error. go-git then reports `ng <ref> <reason>` for all refs, which gives atomic-push semantics. Mark the pack orphaned for GC.
- go-git's own `updateReferences` then succeeds against the in-memory snapshot, and report-status says `ok`.
- This sidesteps the non-CAS create and delete paths in go-git (1.3), and gives you atomicity even though `atomic` is not advertised.
- **Alternative:** `git receive-pack --stateless-rpc` with `receive.unpackLimit=1` and a pre-receive hook that calls back into Hangar (the Gitea pattern). It needs more moving parts: a hook binary, an internal API and quarantine env handling.

**Server-side commits (HF commit API, implemented by the other agent):**
- Build blobs (pointer files for LFS), trees and the commit with go-git into a `memory.Storage`.
- Encode a pack containing **only the new objects** (no delta, or a small window).
- Then follow the push path: upload the pack, then CAS `refs/heads/<rev>` from `parentCommit` (or the current tip) to the new SHA.

**Mirror ingest:**
- Fetch from `https://huggingface.co/<repo>` with go-git v6 (client) or the `git` CLI. Store the received pack **verbatim** as a Hangar pack, so upstream deltas are kept and SHAs are identical by construction.
- Choose refs explicitly: `refs/heads/*`, `refs/tags/*`, and optionally `refs/pr/*` and `refs/convert/*`. A plain clone only takes `refs/heads/*`. jina has 53 `refs/pr/*` [test].
- LFS objects for unselected files are recorded in `repo_lfs` as `absent`. See §3.8 for how downloads of absent objects behave.

**Compaction and GC (background, single leader via `pg_try_advisory_lock`):**
- Every push and every API commit adds a pack. When the pack count exceeds a threshold, materialise a view and run `git repack -a -d --write-bitmap-index` (or `git multi-pack-index write --bitmap`).
- Upload the new pack. Swap the manifest in one transaction: new pack `live`, old packs `retired`. Delete retired and orphaned `pending` packs only after a grace period longer than the maximum request time.
- Only prune objects unreachable from **all** refs, including `refs/pr/*`. Never prune mirrors.

### 2.3 Trade-offs of the recommendation

- **Gains:**
  - Complete and fast fetch protocol: v2, filter, shallow, bitmaps, delta reuse.
  - Push and CAS logic in Go with DB transactions.
  - The cache dir is a valid bare repo, so you can debug with `git` and could swap engines per operation.
- **Costs:**
  - A `git` binary in the image, and a subprocess per fetch.
  - Two git implementations to keep consistent.
  - Local disk for the cache, sized to the working set; model-repo git data is small.
  - First request per repo per replica pays the pack downloads.
- **If "no git binary" is a hard requirement:** use go-git `UploadPack`. Set `pack.window=0` or `SkipDeltaCompression`, or better, store packs from compaction and **cache the generated full-clone pack** per ref-tip set, because most model clones have no `have`s. Pin protocol v2. You lose `--filter` and v0 shallow until #2011 is fixed.

### 2.4 Concurrency: two pushes to one branch on different replicas

Both replicas upload their own pack (distinct content names, so no clobbering), then race on the DB:

```sql
BEGIN;
-- update (old != 0, new != 0)
UPDATE refs SET target_sha1=$new, updated_at=now()
 WHERE repo_id=$r AND name=$ref AND target_sha1=$old;          -- require rowcount = 1
-- create (old = 0)
INSERT INTO refs(repo_id,name,target_sha1) VALUES ($r,$ref,$new)
 ON CONFLICT DO NOTHING;                                       -- require rowcount = 1
-- delete (new = 0)
DELETE FROM refs WHERE repo_id=$r AND name=$ref AND target_sha1=$old;  -- require rowcount = 1
UPDATE packs SET state='live' WHERE repo_id=$r AND pack_sha1=$p;
COMMIT;   -- any rowcount 0 => ROLLBACK, report "reference has changed concurrently" / "fetch first"
```

- `READ COMMITTED` is enough, because each statement is a single-row conditional write.
- Lock order (sort the refs) avoids deadlocks on multi-ref pushes.
- Force pushes are just CAS with the old value the client saw in the advertisement. The client sends that `old` in the command, which is how `--force-with-lease` works.
- The loser's pack is orphaned and later GC'd.
- The HF commit API should use the same CAS function, so git pushes and API commits serialise correctly against each other.

---

## 3. git-lfs server API (what Hangar must implement)

Sources: git-lfs `docs/api/{batch,basic-transfers,locking,server-discovery,authentication}.md` and `docs/spec.md` [doc]; client code [src].

### 3.1 Discovery

- The LFS endpoint is `<remote>.git/info/lfs`. This applies whether or not the remote URL ends in `.git`.
- So `https://hangar/<ns>/<name>` gives `https://hangar/<ns>/<name>.git/info/lfs`, and datasets give `https://hangar/datasets/<ns>/<name>.git/info/lfs`.
- Overrides are `lfs.url`, `remote.<name>.lfsurl`, or a committed `.lfsconfig`.
- Batch: `POST <lfs>/objects/batch`. Locks: `<lfs>/locks`, `<lfs>/locks/verify`.
- **The router must accept both `<repo>` and `<repo>.git` for git paths, and `<repo>.git/info/lfs/...` for LFS.** HF works with and without `.git` [test: `git ls-remote https://huggingface.co/hf-internal-testing/tiny-random-bert.git`].

### 3.2 Batch API

- `POST`, with `Accept` and `Content-Type: application/vnd.git-lfs+json`. Also accept `; charset=utf-8`.
- **Request:** `{operation: "download"|"upload", transfers?: ["basic", ...], ref?: {name}, objects: [{oid, size}], hash_algo?: "sha256"}`.
- **Response:** `{transfer?: "basic", objects: [{oid, size, authenticated?, actions?: {download|upload|verify: {href, header?, expires_in?, expires_at?}}, error?: {code, message}}], hash_algo?}`.
- **Status codes:**
  - Always 200, with per-object errors: 404 missing, 410 removed, 422 invalid, 409 hash_algo.
  - Request-level 401 needs `LFS-Authenticate: Basic realm="..."`. Request-level 403 means read but no write; 404 means no repo; 422; 413 too many objects; 429 with `Retry-After`; 507; 509.
  - A response to an upload request that omits `actions` means "already have it". **Use that for dedupe.**
- The client batches **100 objects** per request (`defaultBatchSize = 100` in `tq/transfer_queue.go:19`).
- `ref` is optional (added in v2.4). Use it for per-ref write ACLs.

### 3.3 Basic transfer

- **Download:** the client sends `GET href` with the action headers.
- **Upload:** a single `PUT href` with **`Content-Length: <size>`** (`basic_upload.go`).
  - The client sets `Content-Type` from `http.DetectContentType` of the first 512 bytes, **unless the action `header` already sets `Content-Type`** (`setContentTypeFor`).
  - For presigned S3 PUTs that sign `Content-Type`, put `"Content-Type": "application/octet-stream"` in the action header and sign the same value. Otherwise leave it out of the signed headers.
- **`authenticated: true`** makes the adapter use plain `apiClient.Do(req)`, so no git credentials are attached (`tq/adapterbase.go:227-233`). **Set it for presigned URLs.** Otherwise git-lfs may add `Authorization: Basic`, and S3 rejects a request that carries both a signature and an Authorization header [unverified: exact S3 error text].
- **Verify:** if the upload response includes `actions.verify`, the client `POST`s `{oid, size}` there after the upload. 200 means the object exists. Use verify to `HEAD` the S3 key, or to finalise a staged upload, then mark the object `present`.
- `transfers` in the client: `basic` is the default. `ssh` and `lfs-standalone-file` exist. `tus` is opt-in (`lfs.tustransfers`). Custom agents use `lfs.customtransfer.<name>.*` [src: `tq/manifest.go`].

### 3.4 Locking API, and how to keep `git push` quiet

Endpoints [doc]:
- `POST /locks` (create)
- `GET /locks?path=&id=&cursor=&limit=&refspec=` (list)
- `POST /locks/verify` (`{ref:{name}, cursor?, limit?}` returns `{ours, theirs, next_cursor?}`)
- `POST /locks/:id/unlock`

Client behaviour on push, from `commands/lockverifier.go` and `locking/locks.go:271-275`:

| Server answer to `POST /locks/verify` | Client behaviour (lfs.<url>.locksverify unset) |
|---|---|
| **404 or 501** | Mapped to `NotImplementedError`, so `disableFor(url)` **silently** writes `lfs.<url>.locksverify=false` to the local git config. No warning, and the push continues. |
| 403 | Auth error: prints `warning: Authentication error` (fatal if verify is explicitly enabled) |
| other error | Prints `Remote "origin" does not support the Git LFS locking API. Consider disabling it with: $ git config lfs.<url>.locksverify false` |
| 200 `{ours:[],theirs:[]}` | Prints `Locking support detected on remote "origin". Consider enabling it with: $ git config lfs.<url>.locksverify true` on **every** push until the user sets it. GitHub is the only host hardcoded as known to support locking (`hostsWithKnownLockingSupport`). |

- **Recommendation: answer 404 with a JSON `{message}` from every `/locks*` endpoint in v1.**
- HF does the same: anonymous `POST .../info/lfs/locks/verify` and `GET .../locks` both return **404** [test; authenticated behaviour unverified].

### 3.5 Authentication

- LFS uses HTTP Basic, taking credentials from the git credential helper for the same host. `credential.useHttpPath` gives per-path credentials. Credentials can also be embedded in the URL [doc].
- HF: the username is anything and the **user access token is the password** ("used in place of a password to access the Hugging Face Hub with git or with basic authentication") [doc: hub/security-tokens].
- For git smart HTTP, unauthenticated `info/refs?service=git-receive-pack` must return `401` with `WWW-Authenticate: Basic realm="..."`, so git asks the credential helper. HF does exactly this [test].
- Public reads should return 200 without auth.
- Action `header` values (for example `Authorization: Bearer <short-lived token>`) are sent as given. If `Authorization` is already set, git-lfs does not add its own (`lfsapi/auth.go:39-43`).

### 3.6 Pointer files and `.gitattributes` [doc: spec.md]

```
version https://git-lfs.github.com/spec/v1
oid sha256:<64 lowercase hex>
size <bytes>
```

- UTF-8, `key value\n` lines, `version` first, then keys sorted, with a trailing `\n`. Under 1024 bytes. Exactly one valid encoding. Preserve unknown keys. An empty file is its own pointer.
- Hangar's commit API must emit byte-exact pointers, or blob SHAs will differ from what git-lfs would produce. Test with `git lfs pointer --file=X --pointer=Y`.
- Server-side parse rule: a blob of fewer than 1024 bytes that starts with `version https://git-lfs.github.com/spec/v1\n` (or the legacy `https://hawser.github.com/spec/v1`) is a pointer.
- `.gitattributes` lines are `<pattern> filter=lfs diff=lfs merge=lfs -text`. HF's default template has about 35 patterns: `*.bin`, `*.h5`, `*.onnx`, `*.pt`, `*.safetensors`, `*tfevents*` and so on [test: tiny-random-bert]. Copy HF's default list for new repos.

### 3.7 HF-specific deviations

- **URLs:** clone URLs are `https://huggingface.co/<ns>/<name>` for models and `https://huggingface.co/datasets/<ns>/<name>` for datasets (verified with ls-remote; spaces would be `/spaces/`). PR refs live at `refs/pr/<n>`, and datasets also have `refs/convert/parquet` [test: refs/pr/*; refs/convert from memory, unverified].
- **Non-LFS file limit:** "Do you have files larger than 10MB? Those files should be tracked with git-xet" [doc: hub/repositories-getting-started]. Hangar can enforce a similar limit in `PreReceive`.
- **`multipart` custom transfer** (huggingface_hub `cli/lfs.py` and `lfs.py`):
  - The batch request offers `["basic", "multipart"]`.
  - For a multipart upload, `actions.upload.href` is the **completion URL**, and `actions.upload.header` contains `chunk_size` plus zero-padded part-number keys (`"00001": <presigned PUT url>`, ...). The client checks that the part count equals `ceil(size/chunk_size)`.
  - The client PUTs each chunk, collects the `ETag`s, then `POST`s `{oid, parts:[{partNumber, etag}]}` to the href.
  - Git users opt in with `hf lfs-enable-largefiles .`, which sets `lfs.customtransfer.multipart.path hf`. It exists to "enable upload of files > 5GB" [src].
  - This maps straight onto S3 `CreateMultipartUpload`, `PresignUploadPart` and `CompleteMultipartUpload`.
- **Xet:**
  - A download batch against a Xet-backed repo (`jinaai/jina-embeddings-v3`) returned `transfer: "basic"`, `authenticated: true`, and a `download.href` on `cas-bridge.xethub-eu.hf.co` (the LFS bridge) [test].
  - `git lfs pull --include onnx/model.onnx` with plain git-lfs 3.7.1 downloaded the 1.5 MB file, and its sha256 matched the pointer [test].
  - The docs say "Uploads from legacy / non‑Xet‑aware clients still follow the standard Git LFS path, even if the repo is already Xet-backed… a background process automatically migrates" [doc: hub/xet/legacy-git-lfs]. So **git-xet is recommended but not required for push** [doc; not tested, as I had no write token].
  - The `xetHash` in the tree API and `X-Xet-Hash` on resolve are extra fields. Hangar can ignore them in v1.

### 3.8 Absent LFS objects on mirrors

- For `download` of an `absent` oid, the options are:
  - (a) Return a per-object `error: {code: 404}`. The smudge fails, and users must clone with `GIT_LFS_SKIP_SMUDGE=1`.
  - (b) **Pull-through:** proxy the batch to `https://huggingface.co/<repo>.git/info/lfs/objects/batch` and return upstream's href directly. This is zero-copy; in my test the anonymous HF batch returned an href with no headers.
  - (c) Fetch into the store on demand.
- (b) and (c) are product decisions. Document which one you pick.

---

## 4. Mirroring feasibility [test]

| Check | Result |
|---|---|
| `GIT_LFS_SKIP_SMUDGE=1 git clone https://huggingface.co/hf-internal-testing/tiny-random-bert` | 2.1 s, `.git` = **600 KiB**, 6 commits. HEAD `f171d7baecaf37b5da5a3616d8833b9969753535` **equals** `api/models/... .sha`. |
| Same for `jinaai/jina-embeddings-v3` | 12.6 s wall, `.git` = **16 MiB** (1 pack, 571 objects, 15.15 MiB; mostly about 67 versions of a 730 KB README), 67 commits, 58 remote refs. HEAD `ab036b023d30b4d1138c4c3bfa9f0c445ab455d6` **equals** API `sha`. The LFS files it points at total more than 7 GB (model.safetensors 1.1 GB, onnx/model.onnx_data 2.3 GB, …). |
| Pointer files | `git cat-file -p HEAD:model.safetensors` gives `version https://git-lfs.github.com/spec/v1` / `oid sha256:17ca06ef…7f23` / `size 1144685320` |
| go-git v6 **client** `Clone(memory, Mirror: true)` of jina | OK. HEAD `ab036b02…` matches, 67 commits, 843 objects (the mirror includes `refs/pr/*`). 34.7 s wall; the network varied, so this is not a fair comparison with the CLI. On this Mac the Go binary needed `GODEBUG=netdns=cgo` because of the sandbox DNS. |
| HF advertisement v0 | `multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed allow-tip-sha1-in-want allow-reachable-sha1-in-want no-done symref=HEAD:refs/heads/main filter object-format=sha1 agent=git/2.53.0-rc0…` |
| HF advertisement v2 | `ls-refs=unborn`, `fetch=shallow wait-for-done filter`, `server-option`, `object-format=sha1` |
| `--depth 1` clone of jina | OK, 1 commit, `.git` = 272 KiB |
| `--filter=blob:none --no-checkout` | OK, `.git` = **180 KiB**, all 67 commits and their trees, `remote.origin.promisor=true`. Lazy blob fetch (`cat-file HEAD:config.json`) works. |
| `--filter=tree:0` | OK, 164 KiB |

**Conclusion:**
- Mirroring the full git history is cheap, SHA-exact and LFS-free.
- `allow-reachable-sha1-in-want` lets a mirror fetch a specific revision.
- **Mirror full blobs, not `blob:none`.** Hangar has to serve complete packs to its own clients, and small blobs are tiny next to the LFS files.

## 5. Do the oids match? [test]

For `jinaai/jina-embeddings-v3` `model.safetensors`:

| Source | Value |
|---|---|
| pointer `oid` | `17ca06efd886a065d0081912b04c9e27ef5086a9dd09659cce32aa9c84587f23` |
| tree API `lfs.oid` | `17ca06ef…7f23` (`size` 1144685320, `pointerSize` 135; `oid` = git blob SHA `55b748b6…` of the pointer) |
| `HEAD /resolve/main/model.safetensors` | `302`, `X-Linked-Etag: "17ca06ef…7f23"`, `X-Linked-Size: 1144685320`, `X-Repo-Commit: ab036b02…`, `X-Xet-Hash: ef4106…`, `Location: https://cas-bridge.xethub-eu.hf.co/...` |

Also checked on tiny-random-bert `onnx/model.onnx`: pointer `083459756ab4…f93`, tree API `lfs.oid` identical.

**They all match.** The LFS sha256 is the canonical content id: use it as the blob-store key.

The tree API `oid` for an LFS file is the git blob SHA of the *pointer*, not of the content.

## 6. S3 multipart, presigning and streaming

**Limits** [doc: AWS S3 User Guide, "Amazon S3 multipart upload limits"]:
- Max object size **48.8 TiB**.
- **10,000 parts**, numbered 1 to 10,000.
- Part size **5 MiB to 5 GiB**, with no minimum for the last part.
- ListParts returns at most 1,000 per page.
- A single PUT is still at most 5 GiB (minio-go `defaultMaxSinglePutObjectSize`).
- minio-go's own constant `maxMultipartPutObjectSize` is still **5 TiB** [src: constants.go]. That only matters for its high-level `PutObject`.

**Presigned part URLs:**
- **aws-sdk-go-v2:** `s3.NewPresignClient(c).PresignUploadPart(ctx, &s3.UploadPartInput{Bucket, Key, UploadId, PartNumber})` exists [src: `service/s3/api_op_UploadPart.go:664`]. `CreateMultipartUpload` and `CompleteMultipartUpload` have **no** presign helpers, so call them server-side. That fits: Hangar creates the upload and completes it in the HF `multipart` completion handler.
  - `feature/s3/manager` is deprecated in favour of `feature/s3/transfermanager` (discussion #3306).
- **minio-go:** there is no dedicated part presigner. Use `Client.Presign(ctx, "PUT", bucket, key, expiry, url.Values{"partNumber": {"N"}, "uploadId": {id}})`. `Presign` accepts arbitrary `reqParams` (max expiry 7 days) [src: `api-presigned.go:111`]. Use `minio.Core{}` for `NewMultipartUpload`, `PutObjectPart`, `CompleteMultipartUpload`, `AbortMultipartUpload` and `ListObjectParts` [src: `core.go`]. [unverified: I did not run a presigned-part round trip against MinIO or AWS.]

**Streaming a multi-GB `basic` upload through Hangar without touching disk** (needed when the object is over 5 GiB, or when you do not want to hand out presigned URLs):
1. `PUT /lfs/upload/<oid>` arrives with `Content-Length = size`. Reject if it is not equal to the batch-declared size.
2. Pick `partSize = max(16 MiB, ceil(size/10000))`, rounded up to a MiB.
3. `NewMultipartUpload` to the **final key** `lfs/sha256/.../<oid>`.
4. Loop: `io.ReadFull(body, buf[:partSize])` into a `sha256` hasher, then `PutObjectPart`. Memory is `partSize × concurrency` (use a pool of 2 to 4 buffers for pipelining).
5. At EOF: if `hex(sha256) == oid` and bytes == size, `CompleteMultipartUpload`. Otherwise `AbortMultipartUpload` and return 422.
   - An incomplete multipart upload is invisible, so a corrupt object never shows up under the content-addressed key and no copy or rename is needed.
6. Add a bucket lifecycle rule `AbortIncompleteMultipartUpload` (for example after 1 day) for crashed replicas.

Libraries:
- minio-go `PutObject` with a known size and a non-`ReaderAt` reader already does this internally (`putObjectMultipartStreamOptionalChecksum`, buffering one part in memory), but you cannot abort it on a hash mismatch before it completes. **Use the Core API.**
- aws-sdk-go-v2 `transfermanager.UploadObject` streams an `io.Reader` with pooled part buffers (`PartSizeBytes`, `Concurrency`). It has the same completion-control issue.

**Presigned direct-to-S3 `basic` PUT** is only possible up to 5 GiB on AWS. Some S3-compatible stores allow larger single PUTs; minio-go's comments say MinIO does.

Routing by size:
- Up to 5 GiB: presigned PUT, then the `verify` action does `HEAD` plus a size check. Plain S3 does not check sha256 for you. To get a server-side integrity check, sign `x-amz-checksum-sha256` (base64 of the oid bytes) into the presigned PUT [unverified: SigV4 presign with that checksum header; test it].
- Over 5 GiB: proxy-stream through Hangar, or HF `multipart`.

**FS backend:** write to `lfs/tmp/<uuid>` on the same filesystem while hashing, then `fsync`, verify, and `rename` to the final key, which is atomic. Multiple replicas on FS need a shared RWX volume.

---

## Recommendation

1. **Git object storage:** immutable packfiles (+ `.idx`, `.rev`) in S3/FS under `git/<repo_id>/pack/`. Refs, the pack manifest and LFS metadata live in Postgres. Never store loose objects remotely.
2. **Replicas:**
   - Stateless apart from a disposable local pack cache.
   - Each request builds a temporary view repo (alternates to the cache, plus `packed-refs` from one DB snapshot).
   - Rule: pack bytes are durable, then the DB transaction (CAS refs + pack `live`), then the ref is visible.
3. **Fetch:** `git upload-pack --stateless-rpc` (git binary in the image), with `uploadpack.allowFilter=true` and `GIT_PROTOCOL` passed through.
4. **Push:** go-git v6 `transport.ReceivePack` (pin `v6.0.0-beta.1` or a commit) with an in-memory ref snapshot. `PreReceive` uploads the pack, checks LFS pointers, then runs the CAS transaction across all refs. This gives atomic all-or-nothing per push and the race handling verified above.
5. **Server-side commits and mirrors:** use go-git (memory storer, then an encoded pack) and the same CAS function. Mirrors store upstream packs verbatim (SHA-exact) and record LFS oids as `absent` unless selected.
6. **LFS:**
   - Batch + `basic`, with presigned S3 URLs and `authenticated: true` (an action `Content-Type` header when it is signed). `verify` action to finalise.
   - Streamed multipart proxy for objects over 5 GiB, and optionally HF `multipart`.
   - `/locks*` return 404 JSON.
   - Router serves `<repo>[.git]/info/refs`, `.../git-upload-pack`, `.../git-receive-pack` and `<repo>.git/info/lfs/...` for models, `datasets/` and `spaces/`.
7. **Background:** a leader-elected compactor (`git repack -a -d --write-bitmap-index`), manifest swap, grace-period deletion, and orphan `pending` pack cleanup.
8. **Fallback if a git binary is unacceptable:** go-git `UploadPack` with `pack.window=0`, a full-clone pack cache, and v2 only. Accept no `--filter` and the go-git issues listed above.

## Risks

- **go-git v6 is beta.** The API may change before GA; pin it and wrap it behind an internal interface. Upload-pack bugs (#2011 v0 shallow, #2519 v2 capability parsing) are avoided by not using go-git for fetch. Receive-pack still depends on go-git correctness: unpack, refname validation and report-status. Run a conformance suite with real `git` (push, force, delete, tags, push-options, large packs) in CI.
- **go-git is not thread-safe (#773).** Use one storer per request and no shared caches.
- **`atomic` is not advertised.** `git push --atomic` fails client-side, even though Hangar's transaction makes every push atomic. Patching go-git to advertise it is a small upstream contribution.
- **Two git implementations.** git's and go-git's views of a repo (refnames, object-format, config) could diverge. Keep the view repo minimal and SHA-1 only. HF is `object-format=sha1`.
- **Pack proliferation.** Every API commit and push adds a pack. Lookups and `upload-pack` slow down with hundreds of packs until compaction runs. Compaction must be safe against concurrent readers: retire, then grace period, then delete.
- **Cache cold start and disk.** First access per replica downloads all packs for the repo, which is fine for repos of 16 MiB or less but large for datasets with huge non-LFS git data. Bound the cache with LRU and expose metrics.
- **SQLite and horizontal scale are incompatible** unless all replicas share one writer. SQLite over a network filesystem is unsafe. Treat SQLite as single-replica mode only.
- **FS backend with multiple replicas** needs a shared RWX volume. Rename atomicity on NFS-like filesystems needs testing.
- **LFS integrity on presigned PUT.** S3 does not check sha256 unless you sign a checksum. The `verify` action plus `HEAD` only checks size. Re-hash asynchronously, or route uploads through Hangar, if integrity matters.
- **Absent LFS objects on mirrors** break checkout (smudge errors) unless you choose pull-through. That is a UX decision to make before v1.
- **The 5 GiB `basic` ceiling on AWS.** Large GGUF and safetensors shards over 5 GiB need the proxy-stream path, which costs replica bandwidth and long-lived requests (set ingress timeouts), or the HF `multipart` agent, which needs the `hf` CLI on clients.
- **Lock verification hint.** If you ever implement locking (return 200), every push prints a hint until users set `locksverify`.
- **Unverified items**, which need a test before relying on them:
  - S3 presigned PUT with `x-amz-checksum-sha256`.
  - minio-go presigned part upload round trip.
  - HF authenticated behaviour of `/locks/verify`.
  - Plain git-lfs push to an HF Xet repo.
  - `refs/convert/parquet` naming.
  - Whether `git upload-pack` uses bitmaps that live only in an alternate (assume no: put bitmaps in the compacted pack and read them via the view's alternates, or test).

## Sources

- go-git: https://github.com/go-git/go-git (main@bb92758a; v5.19.3). Files:
  - `backend/{backend,http}.go`
  - `plumbing/transport/{serve,upload_pack,receive_pack}.go`
  - `plumbing/format/packfile/{common,encoder,delta_selector}.go`
  - `plumbing/storer/*.go`, `storage/storer.go`, `storage/filesystem/storage.go`
  - `COMPATIBILITY.md`, `_examples/http-server/main.go`, `_examples/storage/README.md`
  - v5 `plumbing/transport/server/server.go`
- go-git issues: #2011, #2519, #1628, #773, #1601 (https://github.com/go-git/go-git/issues/NNNN); release notes for v6.0.0-beta.1.
- git-lfs: https://github.com/git-lfs/git-lfs (main@0043a645).
  - Docs: `docs/api/{README,batch,basic-transfers,locking,server-discovery,authentication}.md`, `docs/spec.md`.
  - Code: `commands/lockverifier.go`, `commands/uploader.go`, `locking/locks.go`, `lfshttp/errors.go`, `lfsapi/auth.go`, `tq/{adapterbase,basic_upload,transfer_queue,manifest}.go`.
- Gitea `routers/web/repo/githttp.go`; Soft Serve `pkg/git/service.go`; Keybase `go/kbfs/kbfsgit/runner.go`.
- libgit2 `src/libgit2/transports/`; git2go README and releases; git-lfs/gitobj README.
- GitLab Gitaly docs: https://docs.gitlab.com/administration/gitaly/
- HF docs:
  - https://huggingface.co/docs/hub/xet/using-xet-storage
  - https://huggingface.co/docs/hub/xet/legacy-git-lfs
  - https://huggingface.co/docs/hub/repositories-getting-started
  - https://huggingface.co/docs/hub/security-tokens
  - https://huggingface.co/docs/hub/storage-limits
- huggingface_hub `src/huggingface_hub/{lfs.py,cli/lfs.py}`.
- AWS S3 multipart limits: https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html
- aws-sdk-go-v2 `service/s3/api_op_UploadPart.go`, `feature/s3/manager/upload.go`, `feature/s3/transfermanager/api_op_UploadObject.go`.
- minio-go `api-presigned.go`, `core.go`, `constants.go`, `api-put-object*.go`.
- Empirical: HF endpoints `https://huggingface.co/{hf-internal-testing/tiny-random-bert,jinaai/jina-embeddings-v3}` (`info/refs`, LFS batch, `api/models`, `tree`, `resolve`), queried 2026-10-13.
