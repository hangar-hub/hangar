# Interface coverage

Phase 0 checked that the interfaces have every method the Hub API, git and Xet contracts need. Each row is a requirement from [`03-hf-api.md`](../spec/03-hf-api.md), [`04-git.md`](../spec/04-git.md) or the research contracts ([`hf-hub-api-contract.md`](../research/hf-hub-api-contract.md), [`git-server-research.md`](../research/git-server-research.md), [`xet-upload-research.md`](../research/xet-upload-research.md)). `internal/contracttest/coverage_test.go` holds the same table and fails if a method is missing or a requirement is not listed here.

| Requirement | Source | Interface methods | Implemented by |
|---|---|---|---|
| `HF-RESOLVE` | contract §1.2, §1.5, §1.6 | `repo.Service` Get, Resolve, Entry, OpenFile; `repo.LFSStore` PresignGet, Open; `blob.Store` PresignGet; `auth.Authz` Can; `community.Service` RecordDownload | W4 handler; W2, W1, W5, W7 services |
| `HF-REPO-INFO` | contract §2.1, §2.2 | `repo.Service` Get, Resolve, Tree; `index.Indexer` Get; `scan.Scanner` Results; `community.Service` Likes, Downloads | W4 |
| `HF-TREE` | contract §3.1 | `repo.Service` Tree (with `TreeOpts.After` and `Limit` for cursor pages), Entry | W4, W2 |
| `HF-PATHS-INFO` | contract §3.2 | `repo.Service` Entries | W4, W2 |
| `HF-REFS` | contract §4.1 | `repo.Service` Refs | W4, W2 |
| `HF-COMMITS` | contract §4.2 | `repo.Service` Log | W4, W2 |
| `HF-BRANCH` | contract §4.3, §4.4 | `repo.Service` CreateBranch, DeleteBranch | W4, W2 |
| `HF-TAG` | contract §4.5, §4.6 | `repo.Service` CreateTag, DeleteTag | W4, W2 |
| `HF-LIST` | contract §5 | `repo.Service` List | W4, W2 |
| `HF-WHOAMI` | contract §6.1, §6.2 | `auth.Authenticator` / `httpx.Authenticator` Authenticate; `org.Service` MembershipsOf | W4, W5 |
| `HF-DEVICE-LOGIN` | contract §6.3 | `auth.DeviceFlow` Start, Approve, Deny, Poll; `auth.Tokens` Create | W4 mounts `/oauth/*`; W5 implements |
| `HF-CREATE` | contract §7.1 | `repo.Service` Create; `org.Service` Get; `auth.Authz` Can | W4, W2, W5 |
| `HF-DELETE` | contract §7.2 | `repo.Service` Delete | W4, W2 |
| `HF-SETTINGS` | contract §7.3 | `repo.Service` UpdateSettings | W4, W2 |
| `HF-MOVE` | contract §7.3b, §10 | `repo.Service` Rename, Transfer, Get (returns the canonical ID, so handlers can answer 307) | W4, W2 |
| `HF-PREUPLOAD` | contract §7.5 | `repo.Service` Resolve, Entries, OpenFile (reads `.gitattributes`) | W4 |
| `HF-LFS-UPLOAD` | contract §7.6 | `repo.LFSStore` StatMany, CreateUpload, GetUpload, PresignPut, PresignPart, PutPart, CompleteUpload, AbortUpload, Verify | W3 handler; W2 store; W1 Blob store |
| `HF-COMMIT` | contract §7.7 | `repo.Service` Commit; `repo.LFSStore` Stat; `community.Service` Create (for `create_pr=1`, then Commit to `refs/pr/N` with `CreateFrom`) | W4, W2, W7 |
| `HF-DISCUSSIONS` | contract §7.8, §11.2 | `community.Service` List, Get, Create, Comment, SetStatus, Rename, Merge | W4 handler; W7 service |
| `HF-COLLECTIONS` | BROWSE-5 | `collections.Service` List, Get, Create, Update, Delete, AddItem, UpdateItem, DeleteItem | W4 handler; W7 service |
| `HF-XET-TOKEN` | Xet research §1.3 | `xet.Ingestor` IssueWriteToken | W4 mounts the token route; W11 implements |
| `HF-XET-CAS` | Xet research §2 | `xet.Ingestor` VerifyToken, PutXorb, AcceptShard | W11 |
| `HF-XET-REASSEMBLY` | Xet research §5 | `repo.LFSStore` Ingest, Stat; `blob.Store` Get, Put | W11 |
| `HF-PARQUET` | 14-datasets.md Hub API additions | `datasets.Viewer` Parquet, Croissant | W4 mounts `/api/datasets/{repo}/parquet` and `/croissant`; W10 implements |
| `HF-DATASETS-SERVER` | contract §11.5, DATA-4 | `datasets.Viewer` IsValid, Splits, FirstRows, Rows, Search, Filter, Info, Size, Statistics | W10 |
| `HF-SAFETENSORS-GUARD` | 03-hf-api.md transformers safetensors auto-conversion | `repo.Service` Tree (to see whether safetensors weights exist) | W4 |
| `GIT-FETCH` | 04-git.md Fetch and clone | `repo.Service` OpenView, Get; `auth.Authz` Can | W3 |
| `GIT-PUSH` | 04-git.md Push | `repo.Service` OpenView, ApplyPack; `repo.LFSStore` StatMany; `auth.Authz` Can | W3 handler; W2 ApplyPack |
| `GIT-REF-CAS` | 04-git.md Ref updates | `repo.Service` UpdateRefs | W2 |
| `GIT-LFS-DOWNLOAD` | 04-git.md Batch, git research §3.2 | `repo.Service` LFSRefs (added in phase 0, see below); `repo.LFSStore` PresignGet, Open, StatMany | W3 |
| `GIT-LFS-VERIFY` | 04-git.md Verify | `repo.LFSStore` Verify | W3 |
| `GIT-MIRROR` | 04-git.md Mirrors | `upstream.Client` FetchPack; `repo.Service` ApplyPack, SetLFSPresence, LFSRefs | W6, W2 |
| `OLLAMA` | 08-local-tools.md | `index.Indexer` Quants, Get; `repo.LFSStore` Open; `auth.OllamaAuth` IssueRegistryToken | W7, W5 |
| `PULL-PLAN` | 06 Pull lifecycle | `upstream.Client` RepoInfo, Tree, FetchPack; `org.Service` EnsureMirrorNamespace; `repo.Service` Create, ApplyPack, SetLFSPresence; `jobs.Queue` Enqueue; `datasets.Layout` Resolve, Files | W6 |
| `PULL-TRANSFER` | 06 Transfer | `upstream.Client` Download; `repo.LFSStore` Ingest, CreateUpload, PutPart, CompleteUpload; `jobs.Limiter` WaitN | W6, W1 |
| `EVENTS` | 10 Webhook events, 05 Audit events | `webhook.Emitter` Emit; `audit.Recorder` Record | Every workstream emits; W7 and W5 implement |

Requirements with no service method: `POST /api/validate-yaml` (contract §7.4) is pure handler logic in W4, and `/locks*` returning 404 (04-git.md) is in W3.

## Gaps the check found, and what phase 0 changed

The interfaces in 02-architecture.md "Core interfaces" are the starting point. The check found these gaps, and phase 0 added the methods:

- `blob.Store.PresignPut`: basic LFS uploads up to 5 GiB use a presigned S3 PUT (04-git.md Batch).
- `blob.Store.ListParts`: resuming multipart uploads needs the parts done so far (native `GET .../uploads/{id}` and the LFS multipart path).
- `repo.Service.Entries`: paths-info answers batches of 500 paths.
- `repo.Service.GetCommit`, `Diff`, `FileDiff`: Commit detail with a file-level diff (REPO-3).
- `repo.Service.CreateBranch`, `DeleteBranch`, `CreateTag`, `DeleteTag`: branch and tag endpoints, with default branch protection in one place.
- `repo.Service.ApplyPack` and `OpenView`: git push and Mirror syncs must write refs through `internal/repo`, the only package allowed to.
- `repo.Service.SetLFSPresence` and `LFSRefs`: Partial mirrors, and LFS downloads limited to objects the Repo references.
- `repo.Service.GetByUUID`, `Purge`, `List`: Jobs work by UUID; the purge Job; Hub search and the Arsenal.
- `repo.LFSStore`: one owner for `lfs_objects` and `uploads`, used by W2, W3, W4, W6 and W11.
- `community.Service`, `collections.Service`, `org.Service`: W4 (batch 1) mounts Hub endpoints whose services W5 and W7 build, so it needs the contracts and fakes now.
- `auth.Tokens`, `auth.DeviceFlow`, `auth.OllamaAuth`: device-code login and Ollama keys cross the W4, W5 and W7 boundaries.
- `jobs.Limiter`: the shared bandwidth budget (PULL-7) used by W6.
