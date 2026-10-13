# 08. Ollama, LM Studio and the `hangar` CLI

## Ollama registry endpoint

Hangar serves GGUF Repos the way the Hub does for `ollama run hf.co/{org}/{repo}:{quant}` ([Hub docs](https://huggingface.co/docs/hub/ollama)), so this works with no Modelfile:

```sh
ollama pull hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

The Ollama client code is the reference: `server/images.go`, `server/auth.go`, `auth/auth.go` and `types/model/name.go` in `github.com/ollama/ollama`. Read them before implementing, and pin the Ollama version tested in `compat/ollama`.

### Names and tags

- Ollama names are `host/namespace/model:tag`, so the URL is `/v2/{ns}/{name}/manifests/{tag}`.
- `tag` is matched case-insensitively against the Repo's GGUF quant groups ([06](06-upstreams-and-pulls.md)), or against a full GGUF filename.
- `latest` (Ollama's default) picks `Q4_K_M` if present, otherwise the closest quant by this order: `Q4_K_M, Q4_K_S, Q5_K_M, Q4_0, Q5_K_S, Q6_K, Q8_0, IQ4_XS, Q3_K_M, BF16, F16`, otherwise the smallest file.
- The chosen file must be present. In a Partial mirror, a missing quant returns `404` with error code `MANIFEST_UNKNOWN` and a message naming the Pull to run; with Pull-through on, it starts the Pull and returns `503` with `Retry-After`.
- Split GGUF files: check whether the pinned Ollama version can pull multi-file models. If it cannot, return a clear error naming the limit.

### Manifest

`GET|HEAD /v2/{ns}/{name}/manifests/{tag}` returns a Docker v2 schema 2 manifest (`application/vnd.docker.distribution.manifest.v2+json`), built on the fly:

| Layer media type | Content |
|---|---|
| `application/vnd.ollama.image.model` | The GGUF file. Digest is `sha256:<LFS oid>`, so no copy is made. |
| `application/vnd.ollama.image.projector` | A matching `mmproj-*.gguf`, if the Repo has one. |
| `application/vnd.ollama.image.template` | The Repo's `template` file if present; otherwise the Ollama template matched from the GGUF `tokenizer.chat_template`. Vendor Ollama's `template/index.json` and `*.gotmpl` files (MIT; keep the licence notice in `third_party/ollama/`) and port its matching function. Omit the layer if nothing matches. |
| `application/vnd.ollama.image.system` | The Repo's `system` file, if present. |
| `application/vnd.ollama.image.params` | The Repo's `params` file (JSON), if present. |
| `application/vnd.ollama.image.license` | The licence text, if the Repo has a `LICENSE` file. |

The config blob (`application/vnd.docker.container.image.v1+json`) is JSON with `model_format: gguf`, `model_family`, `model_families`, `model_type` (parameter size, for example `3.2B`), `file_type` (quant), and `rootfs.diff_ids`. Generate it deterministically from the index so its digest is stable, and store generated blobs in a `ollama_blobs` table (digest, bytes) so `GET /v2/.../blobs/{digest}` can serve them.

### Blobs

`GET|HEAD /v2/{ns}/{name}/blobs/{digest}` serves the layer, with `Content-Length`, `Docker-Content-Digest` and range support.

**Always stream blobs through Hangar, never redirect to a presigned S3 URL.** Ollama's client only follows redirects to the same host, or to a short list of Ollama's own CDN hosts, unless the user passes `--insecure` (`server/images.go`, `makeRequest` and `isAllowedHost`). A cross-host redirect fails the pull.

### HTTP and TLS

Ollama uses HTTPS unless the user passes `--insecure`. Document both: HTTPS with a trusted certificate (recommended), and `ollama pull --insecure` for plain-HTTP LAN Instances.

### Auth

Any Repo the caller cannot read anonymously (internal and private Repos, and public Repos when `anonymous_read` is off) uses this flow, which is Ollama's registry auth as implemented in its client:

1. An unauthenticated request for a Repo the caller cannot read anonymously gets `401` with `WWW-Authenticate: Bearer realm="<public_url>/v2/token",service="<host>",scope="repository:<ns>/<name>:pull"`. The realm host must be the same as the request host, because Ollama refuses a different one.
2. Ollama calls the realm with `service`, `scope`, `ts` and a random `nonce` query parameter, and an `Authorization` header of `<base64 ed25519 public key>:<base64 signature>`. The signed data is `GET,<full realm URL including query>,<base64 of the hex SHA-256 of the empty body>`.
3. Hangar parses the public key (`ssh-ed25519 <key>`), looks it up in `ollama_keys`, verifies the signature, rejects a `ts` more than 5 minutes away and a reused `nonce` (stored for 10 minutes), checks the user can read the Repo, and returns `{"token": "<JWT>"}` valid for 5 minutes and scoped to that Repo.
4. Ollama retries with `Authorization: Bearer <JWT>`.

Public Repos with `anonymous_read` on need no auth.

Push (`ollama push`) is out of scope for v1.0; return `405` with a message pointing to `hangar import --from-ollama`.

## LM Studio

LM Studio reads models from a folder laid out as `<publisher>/<repo>/<files>`. The CLI installs Hangar models there.

- `hangar lmstudio install <ns>/<name>[:<quant>]` downloads into the local Hangar cache (below), then links the files into `<lmstudio-models>/<ns>/<name>/`.
- Link mode: `symlink` (default on macOS and Linux), `hardlink`, or `copy` (`--link-mode`). Symlinks point at the cache's blob files, so disk is used once even when the same model is also used by Python libraries.
- Finding the folder: `--lmstudio-dir`, else LM Studio's configured models folder (read it from LM Studio's settings files; check the current LM Studio version's layout and document the files read), else `~/.lmstudio/models`.
- MLX Repos install as the whole Revision folder; GGUF Repos install the chosen quant group plus any `mmproj-*.gguf`.
- `hangar lmstudio list` shows what is installed from Hangar and whether a newer Revision exists. `hangar lmstudio remove` removes links (and with `--prune`, unreferenced cache blobs).

## Local cache

The CLI caches downloads in the standard Hugging Face cache layout (`$HF_HUB_CACHE`, default `~/.cache/huggingface/hub`, with `models--{ns}--{name}/{blobs,snapshots,refs}`). Python libraries pointed at the same Hangar with `HF_ENDPOINT` reuse the files, and the reverse is true.

## The `hangar` CLI

One binary for server and client. Client commands talk to an Instance through the Hub API and native API.

| Command | What it does |
|---|---|
| `hangar login [--url URL]` | Device-style login: opens the browser to create a Token, or accepts `--token`. Stores `~/.config/hangar/config.yaml` (URL, token; token in the OS keychain where available). |
| `hangar logout`, `hangar whoami` | |
| `hangar env [--shell bash\|zsh\|fish\|powershell]` | Prints `HF_ENDPOINT` and `HF_TOKEN` exports for the current Instance, so Python tools use Hangar. |
| `hangar search <query> [--upstream] [--filter ...]` | Search the Arsenal, or Discover with `--upstream`. |
| `hangar info <repo>` | Metadata, files, quants, Dependencies, arsenal state. |
| `hangar pull <repo>[:<quant>] [--include ...] [--revision ...] [--to DIR]` | If the Repo or files are not in the Arsenal, asks the Instance to Pull them from the Upstream and shows progress; then downloads to the local cache (or `--to DIR`). `--server-only` skips the local download. |
| `hangar push <dir> <repo> [--branch ...] [--message ...] [--create] [--private]` | Uploads a folder as one Commit with resumable multipart uploads. |
| `hangar import ...` | Bulk import ([06](06-upstreams-and-pulls.md)). |
| `hangar lmstudio install\|list\|remove` | See above. |
| `hangar ollama install <repo>[:<quant>]` | Runs `ollama pull <host>/<repo>:<quant>` (adding `--insecure` for `http://` Instances) and checks the Ollama key is registered first, offering to register it. |
| `hangar ollama key register` | Reads `~/.ollama/id_ed25519.pub` and registers it with the Instance. |
| `hangar jobs [list\|watch\|cancel]` | The user's Pulls and other Jobs. |
| `hangar admin ...` | Server-side admin: `migrate`, `create-user`, `reset-password`, `rotate-key`, `gc`, `backup`, `restore`, `reindex`, `doctor` (checks config, database, Blob store, presign, clock skew). These connect to the database and Blob store directly using the server config. |
| `hangar server`, `hangar worker` | Run the Instance ([02](02-architecture.md)). |
| `hangar config reference` | Prints the configuration reference. |
| `hangar healthcheck` | Calls `/readyz` on localhost and exits 0 or 1; used by the container `HEALTHCHECK`. |
| `hangar version` | |

Output: human-readable tables by default, `--json` for scripting on every command. Exit codes: 0 success, 1 error, 2 usage error, 3 not found, 4 auth error.
