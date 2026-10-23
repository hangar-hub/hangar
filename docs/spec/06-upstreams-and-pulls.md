# 06. Upstreams, Pulls, Discover and Dependencies

## Upstreams

An Upstream is any hub that speaks the Hugging Face Hub API: huggingface.co (built in, `id: huggingface`), another Hangar Instance, or a corporate mirror. `internal/upstream` has one client used by Discover, Pulls, Pull-through and Dependency resolution.

- The client uses the Hub HTTP API for metadata and file content, and git smart HTTP for history ([04](04-git.md) decides the exact method).
- Requests carry `User-Agent: hangar/<version>`, honour `Retry-After`, and back off on 429 and 5xx (exponential, full jitter, maximum 5 minutes).
- Auth: the requesting user's upstream token if they saved one, otherwise the Instance token for that Upstream, otherwise anonymous.
- Upstream file downloads never use Xet: the client sends no Xet headers and follows the classic `resolve` redirects ([03](03-hf-api.md)).

### Network policy

Decided in Phase 3 (see [decisions](../build/decisions.md), "Outbound network policy"). Admins choose each Upstream's URL and may point it at the local network, such as another Hangar or a corporate mirror on the LAN, so the policy follows where the Upstream itself is:

- Hangar connects to an Upstream's own host wherever it resolves, except reserved addresses: link-local addresses (which include the cloud metadata service at 169.254.169.254), other metadata services (100.100.100.200, fd00:ec2::254), unspecified addresses (0.0.0.0 reaches the local host), broadcast and multicast. Hangar never connects to these, for Upstreams or webhooks.
- A redirect to another host may reach a private address (loopback, RFC 1918, carrier-grade NAT, unique local) only when the Upstream's own host is private too, as a LAN Hangar that redirects to its LAN object store is. A public Upstream, such as huggingface.co, cannot redirect Hangar into the local network.
- Checks apply to the address each connection is made to, after DNS resolution, so a name that resolves differently between a check and the connection (DNS rebinding) gets nowhere. Requests that may reach private addresses and requests that may not use separate connection pools.
- A refused connection is not retried, and its error names the address and the reason, never the URL.
- A proxy from `HTTP_PROXY` or `HTTPS_PROXY` is trusted: Hangar checks the proxy's address, and the proxy decides the rest.

## Mirrors keep upstream IDs

A Pull of `jinaai/jina-embeddings-v3` creates the Repo `jinaai/jina-embeddings-v3` in Hangar. Keeping the ID is what makes `HF_ENDPOINT` swaps work with no code changes.

- The first Pull into an unknown Namespace creates it as an Organisation with `reserved_mirror = true`, owned by the Instance. Instance admins can hand it to a user or Organisation later.
- If a local Namespace already uses the name, the Pull fails with a clear error unless the admin chooses a target Namespace for that Upstream (setting: `upstreams.<id>.namespace_prefix`, for example `hf-`, giving `hf-jinaai/jina-embeddings-v3`). Document that prefixed Mirrors no longer work as drop-in `HF_ENDPOINT` replacements.
- With several Upstreams, the first one to provide a Repo ID owns it; a Pull of the same ID from a different Upstream fails unless prefixed.
- Mirrors are read-only to git push and the commit API by default, so they stay identical to the Upstream. A Namespace admin can **detach** a Mirror, which turns it into an ordinary Repo that keeps its history.

## Pull lifecycle

A Pull request names an Upstream, a Repo ID, a Revision (default: the upstream default branch) and a file selection (`all`, glob `include`/`exclude` lists, or explicit paths).

1. **Plan** (`pull` Job). Resolve the Revision to an upstream Commit. Fetch the upstream git history into the Repo's git store, so Commits, trees and LFS pointer files match the Upstream exactly ([04](04-git.md)). List the files at that Commit with sizes and LFS SHA-256 values. Apply the selection. Record `repo_lfs` rows: `present = true` for objects already in the Instance (deduplication: no transfer), `present = false` for the rest. Enqueue one `pull.file` Job per missing selected LFS object. Point the Mirror's branch refs at the upstream Commits once planning finishes, so the history is browsable straight away.
2. **Transfer** (`pull.file` Jobs). Stream each object from the Upstream's `resolve` URL into the Blob store (S3 multipart, 64 MiB parts, or a temp file and rename), hashing as it goes. Resume after interruption with HTTP ranges and the parts already uploaded. Reject and retry on SHA-256 or size mismatch. On success set `present = true` and `lfs_objects.verified_at`.
3. **Finish.** When every file Job is done, enqueue `index` and `scan` for the Commit, resolve Dependencies, fire the `pull.completed` webhook, and mark the Pull done. If some files failed after their retries, the Pull ends `partial` with per-file errors and a **Retry failed** action.

Small, non-LFS files come with the git history and need no transfer.

A Pull of more files for an existing Mirror skips step 1's history fetch unless the requested Revision is newer, and only transfers the new objects.

### Network controls

- `pull.bandwidth_limit`: shared token bucket across all workers ([02](02-architecture.md)).
- `pull.concurrency`: maximum `pull.file` Jobs running at once across the Instance.
- `pull.windows`: when set, `pull.file` Jobs only start inside a window; Jobs already running finish their current file part, then wait. The UI shows "Waiting for pull window (01:00-07:00)". A user with `write` can start a Pull immediately with **Pull now**, which ignores windows (not the bandwidth cap), if the Instance setting `pull.allow_override_windows` is true.
- Pulls show bytes done, total, current rate and ETA, per file and overall.

### Tracking

A Mirror with `track: true` gets a `mirror.sync` schedule (default every 6 hours, with jitter). Each run fetches new upstream Commits on the tracked branch. If the head moved, it updates refs and, for files matching the Mirror's selection, pulls new LFS objects. A GGUF quant selection is stored as a quant name, not a file path, so a renamed file of the same quant is still followed. Upstream force-pushes are applied, and the replaced Commits stay reachable through a `refs/hangar/previous/<timestamp>` ref so nothing pinned to them breaks.

### Pull-through

Off by default. When `pull.pull_through.enabled` and the Repo ID matches `allow`:

- A Hub API `model_info` or `resolve` request, or a git `info/refs` request, for a Repo that does not exist starts a Pull of the default branch with no files selected (history only), then answers once planning finishes (wait up to 30 seconds; after that, return 503 with `Retry-After`; the Hub client retries).
- A `resolve` request for a file that is not present starts a `pull.file` Job at high priority and streams bytes to the client as they arrive. Concurrent requests for the same file share the same transfer. If the client is on a different replica, it waits for the object to finish, then serves it.
- Access follows normal rules: Pull-through needs at least `read` on an Instance-level setting `pull.pull_through.min_role` (default: any signed-in user; `anonymous` allowed if the admin sets it).

## Discover

The Discover view and `GET /api/hangar/v1/discover/models` search an Upstream and mark each result with its state in the Arsenal.

- Query parameters map to the Upstream's `GET /api/models`: `search`, `author`, `filter` (tags, including `gguf`, `mlx`, `safetensors`, `onnx`, task and library), `sort` (`trending`, `downloads`, `likes`, `lastModified`), `limit`, cursor pagination.
- Each result carries `arsenal`: `none`, `partial` (some LFS objects present) or `full`, plus the local Revision if it differs from upstream ("Update available").
- Results are cached per query for 5 minutes, per Upstream and per user token (private results must not leak between users).
- Opening a result shows the upstream Model card, file list with sizes, the GGUF quant picker, the Dependencies Hangar would pull, total download size, and a **Pull into arsenal** button.

### GGUF quant picker

Group GGUF files by quantisation, parsed from the filename (case-insensitive tokens such as `Q4_K_M`, `IQ3_XXS`, `Q8_0`, `F16`, `BF16`, `MXFP4`; fall back to the GGUF `general.file_type` after indexing). Split files (`*-00001-of-00003.gguf`) form one entry. Also list `mmproj-*.gguf` projector files and select them automatically with any quant (vision models need them). Show each group's total size. Selection is stored as quant names.

## Datasets

Discover and Pull work the same way for datasets, with dataset facets and Config, Split and Parquet-only selections. See [14](14-datasets.md) (DATA-7, DATA-8).

## Dependencies

After every index of a Commit, `internal/upstream/deps` reads these files and writes `dependencies` rows.

| Source | What it finds | Kind | Required |
|---|---|---|---|
| `config.json` `auto_map` | Values of the form `<repo>--<module>.<Class>` or `<repo>@<revision>--<module>.<Class>`, including list values (for example `AutoTokenizer: [slow, fast]`). Example: `jinaai/jina-embeddings-v3` maps `AutoModel` to `jinaai/xlm-roberta-flash-implementation--modeling_lora.XLMRobertaLoRA`. | `code` | yes |
| `tokenizer_config.json`, `preprocessor_config.json`, `processor_config.json`, `generation_config.json` `auto_map` | same form | `code` | yes |
| `adapter_config.json` `base_model_name_or_path` | the base model of a PEFT adapter, if it looks like a repo ID | `adapter_base` | no |
| Model card `base_model` (string or list) and `base_model_relation` | where the model came from | `card_base_model` | no |
| Model card `datasets` | training datasets | `dataset` | no |
| `modules.json` (sentence-transformers) | module paths; external only if a path is a repo ID | `code` | yes, if external |
| `model_index.json` (diffusers) | component sources if they name another repo | `code` | yes, if external |

Rules:

- A value with no `--` refers to the same Repo and is not a Dependency.
- `code` Dependencies are pulled automatically (all files; they are small) when `pull.dependencies.auto_pull_code` is true, recursively up to `max_depth`, with cycle detection. A Pull is not marked complete until its code Dependencies are complete, so `from_pretrained(..., trust_remote_code=True)` works offline right after.
- Other kinds are listed in the Dependencies panel with their state in the Arsenal and a one-click Pull. `auto_pull_base_models` pulls `adapter_base` automatically when true.
- Uploaded (non-mirror) Repos get Dependencies resolved the same way; missing `code` Dependencies are pulled from the default Upstream if allowed, and shown as a warning if not.
- The Repo page and `GET /api/hangar/v1/repos/{type}/{ns}/{name}/dependencies` show the Dependency graph both ways ("needs" and "needed by").

## Bulk import

`hangar import` adds content that is already on disk, without network transfer.

- `--from-hf-cache [path]`: walks a Hugging Face cache (`~/.cache/huggingface/hub`, layout `models--org--name/{blobs,snapshots,refs}`). For each Repo, plans a Mirror against huggingface.co as in step 1 (history fetch needs the network; with `--offline`, it creates a local Repo with a single Commit instead), then uploads only the blobs that match the planned LFS SHA-256 values. The Mirror is planned at the snapshot `refs/main` names, or else the newest by modification time. Blobs from every cached snapshot are matched by content (SHA-256, or git blob ID for regular files) against the Mirror at that Commit, its default branch and the other cached snapshots' Commits, and uploaded files are pulled into the Mirror at the Commit they matched. Files that match nothing are named with a reason. An existing Mirror that lacks the cached Commit fetches its history first; an import never commits to a Mirror (issues #28 and #29).
- `--from-dir <dir> --repo ns/name`: uploads a directory as a new Commit, as `hangar push` does.
- `--from-lmstudio [path]`: walks LM Studio's models folder (`<publisher>/<repo>/<file>`), matches each folder to the huggingface.co Repo of the same name when it exists (by file SHA-256), and imports as a Partial mirror; otherwise as a local Repo.
- `--from-ollama`: imports Ollama models' GGUF blobs as local Repos with `template`, `system` and `params` files from the Ollama manifest.

`--only <pattern>` (repeatable) limits the cache, LM Studio and Ollama sources to the models it names: a Repo ID for a Hugging Face cache, a `<publisher>/<repo>` folder or its Repo ID for LM Studio, and `model:tag` for Ollama, where a bare `model` means `model:latest`. Patterns use `path.Match` syntax and ignore case. A pattern that matches nothing fails the command with exit code 3 before anything is uploaded.

Uploads go through the normal Hub API, so the CLI works against any Hangar the user can write to.

Symbolic links in a folder that `hangar push` or `hangar import` uploads are followed only into that folder, into the Repo's cache folder for a Hugging Face cache snapshot, or to a blob of a Hugging Face cache (as `hangar lmstudio` links). Any other link, such as `config.json` pointing at `~/.ssh/id_ed25519` in a cloned folder, is skipped and named on stderr, so an upload never sends a file from elsewhere on the machine. Decided in Phase 3 (see [decisions](../build/decisions.md), "Symbolic links in uploaded folders").
