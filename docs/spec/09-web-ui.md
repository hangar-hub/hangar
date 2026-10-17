# 09. Web UI

## Stack and rules

- React with TypeScript (strict), Vite, TanStack Router (file-based routes), TanStack Query, Tailwind CSS, shadcn/ui on Radix primitives, `openapi-fetch` with types from `openapi-typescript` generated from `api/openapi.yaml`. pnpm. Biome for lint and format. Vitest and Testing Library for units, Playwright for end-to-end.
- Built into `internal/web/dist` and embedded with `go:embed`. The Go server serves `index.html` for UI routes and sets a strict Content Security Policy (no inline scripts, `connect-src 'self'` plus the presign host for uploads), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `Cross-Origin-Opener-Policy: same-origin` and a `Permissions-Policy` that turns off camera, microphone, geolocation, payment and device APIs. HSTS is left to the reverse proxy that terminates TLS.
- The UI only uses the native API (`/api/hangar/v1`) plus `resolve` URLs for file content. It never calls Upstreams directly.
- Model cards and other user Markdown: `react-markdown` with `remark-gfm` and `rehype-sanitize`; code highlighting with Shiki, loaded lazily. Raw HTML in cards is sanitised to the Hub's allowed subset. Images from external hosts load only if the Instance setting `ui.external_images` is on (default on), so air-gapped Instances do not try to reach the internet.
- Accessibility: WCAG 2.2 AA. Every action works by keyboard, focus is visible, colour is never the only signal, and Playwright runs `@axe-core/playwright` on every page.
- Responsive: phone (390 px), tablet (1024 px) and desktop (1440 px). Tables become cards on narrow screens.
- Light and dark themes, following the system by default, with a toggle.
- Copy follows the house style and the vocabulary in [01](01-product.md).
- Performance: initial JavaScript under 250 KB gzipped; routes split by page; lists virtualised over 200 rows.

## Visual direction

Calm and dense, closer to a well-made developer tool than a marketing site. A neutral base with one accent colour; monospace for IDs, SHAs, sizes and quant names. The arsenal theme shows up in the landing hero, empty states and the **Pull into arsenal** action, not in decoration. Use the `frontend-design` skill when building it.

Palette (`ui/src/styles.css`): a cool steel neutral and one accent, **Taxiway teal**. Components use the semantic tokens, never raw colours, so the themes stay in one place:

| Token | Light | Dark | Use |
|---|---|---|---|
| `--hg-accent` | `#0b6e64` | `#5fd4c2` | Accent text, links, focus rings, hover on filled buttons |
| `--hg-accent-fill` | `#0b7a6e` | `#2bb3a0` | Primary buttons, switches, the active tab line and the logo's bay line |
| `--hg-accent-ink` | `#ffffff` | `#021a17` | Text on `--hg-accent-fill` |
| `--hg-accent-soft` | `#dff3ef` | `#12332e` | Selected facets and accent badges |

Warnings stay amber (`--hg-warn`), so the accent never reads as a warning. Every pair passes WCAG AA contrast, and Playwright runs axe in both themes.

## Global layout

- Top bar: logo and name, global search (`/` or Cmd-K opens a command palette over models and datasets, in separate groups, Namespaces, Collections and actions such as **New model** and **New dataset**), **Models**, **Datasets**, **Discover**, **Collections**, Jobs indicator (count of running Pulls, opens a drawer with progress), user menu. Models and datasets are separate sections: no list mixes the two types.
- Every page has a stable URL; filters and tabs live in the query string so views can be shared.

## Pages

| Route | Page |
|---|---|
| `/` | Signed out: landing with tagline and sign-in. Signed in: the Models list. |
| `/models` and `/datasets` | Grid or list of the models, or the datasets, the user can see; each list holds one type only, with its own filters ([01](01-product.md) BROWSE-1, [14](14-datasets.md) DATA-11) as a sidebar (sheet on phones), sort (recently updated, name, size, parameters for models, downloads, likes), search box, empty state and **New model** or **New dataset** button. Cards show ID, format badges, parameter count, quant chips (three, then a **+N** chip that names the rest), size present / total, Mirror badge, scan badge, last updated. Cards in a grid row are the same height, with the stats row at the bottom. |
| `/arsenal` | Redirects old links: to `/datasets` for `type=datasets`, to `/models` otherwise, keeping the other query parameters. |
| `/discover` | **Models** and **Datasets** tabs. Upstream picker, search, the same filters, results with **In arsenal**, **Partly in arsenal** and **Update available** badges. A search that is a full Repo ID (`namespace/name`) also offers **Open** for that Repo, because Upstream search does not list every Repo. Opening a result shows the upstream detail panel ([06](06-upstreams-and-pulls.md)) with the quant picker, Dependencies, total size and **Pull into arsenal**. |
| `/datasets/{ns}/{name}/viewer/{config}/{split}` | Dataset Viewer tab ([14](14-datasets.md) DATA-6). Dataset Repo pages show **Dataset card**, **Viewer**, **Files**, **Commits**, **Community** and **Settings** tabs, with Configs, Splits, rows and **Used by** (the models whose cards list the dataset) in the sidebar. A dataset can hold any files, such as sample data for another project: when none is a data file the Viewer reads, the Viewer tab says so and points to the files, the sidebar says there are no Configs, and **Use this dataset** offers only the download snippets (hf CLI, huggingface_hub, git). A dataset with no links to models is normal. |
| `/{ns}/{name}` and `/datasets/{ns}/{name}` | Repo page. Header: ID, copy button, type, visibility, Mirror status (upstream link, last sync, **Sync now**, **Detach**), like, **Use this model**. Tabs: **Model card**, **Files**, **Commits**, **Community**, **Settings** (admins). Sidebar: metadata (format, architecture, parameters, quants, context length, licence, task, library, size), Dependencies (needs and needed by), **Datasets used** (from the Model card's `datasets` metadata), Collections containing it. |
| `/{ns}/{name}/tree/{rev}/{path}` | File browser: breadcrumb, branch and tag picker, table of name, size, last Commit, LFS badge, presence badge (**Not in arsenal**, with a Pull action for Mirrors), scan badge. Upload and **New file** buttons for writers. Download a folder or Revision as zip. |
| `/{ns}/{name}/blob/{rev}/{path}` | File view: highlighted text and code, rendered Markdown, JSON tree, images, safetensors header viewer (tensors, shapes, dtypes, parameter totals), GGUF viewer (metadata keys, tokenizer summary, chat template, tensor list), pickle scan findings. Raw, download, copy permalink, history. Writers can edit small text files in the browser and commit. |
| `/{ns}/{name}/commits/{rev}` | Commit list with author, message, date, SHA. |
| `/{ns}/{name}/commit/{sha}` | Commit detail and file-level diff. |
| `/{ns}/{name}/discussions` and `/discussions/{num}` | Discussions and pull requests: list, thread, comment (Markdown), change status, merge (PRs), files changed. |
| `/{ns}/{name}/settings` | Description, tags, visibility, default branch, **Datasets used** (models that are not Mirrors: pick datasets from the arsenal or name one on an Upstream by ID, committed to the Model card's `datasets` metadata as the Hub stores it), gating, collaborators, webhooks, Mirror settings (selection, tracking, schedule), rename, transfer, delete. |
| `/new` and `/new-dataset` | New Repo: owner, name, visibility, licence, optional initial upload. |
| `/{ns}` | Namespace profile: Repos (models and datasets as separate lists), Collections, members (Organisations). |
| `/collections/{ns}/{slug}` | Collection view and editor (drag to reorder). |
| `/settings/...` | Profile, Tokens (create with scope, restrictions, expiry; shown once), Ollama keys, Upstream tokens, sessions, Organisations. |
| `/admin/...` | Users (create, disable, make admin, reset link), Organisations, Upstreams (add, test connection, Instance token), Pull settings (bandwidth, concurrency, windows, Pull-through), storage (usage by Namespace, quotas, **Run GC**), Jobs (all Jobs, filters, cancel, retry), webhooks, audit log (filter, export CSV), Instance settings (anonymous read, registration, local login, OIDC providers read from config, scan blocking), system info (version, database, Blob store, telemetry status, `doctor` results). |
| `/auth/login`, `/auth/invite/{code}`, `/auth/reset/{code}` | Sign-in (password form and one button per OIDC provider), invite acceptance, password reset. |

## Use this model panel

Tabs with copy buttons, filled in for the current Repo, Revision and Instance URL:

- **Python (transformers)**: `HF_ENDPOINT=...` and `from_pretrained(...)` (with `trust_remote_code=True` when the Repo has code Dependencies)
- **huggingface_hub**: `snapshot_download`
- **hf CLI**: `hf download`
- **git**: `git clone` (with `GIT_LFS_SKIP_SMUDGE=1` variant)
- **Ollama**: `ollama pull <host>/<ns>/<name>:<quant>` per quant (GGUF only)
- **LM Studio**: `hangar lmstudio install ...`
- **vLLM**: `HF_ENDPOINT=... vllm serve <id>`
- **sentence-transformers** and **diffusers** when the library matches
- **datasets** (`load_dataset`, with Config and Split), **pandas** and **Polars** (`hf://datasets/...`) and **DuckDB** (`read_parquet('https://<host>/datasets/<ns>/<name>/resolve/<rev>/<path>')`) for dataset Repos

## Uploads in the browser

- Drag and drop files or a folder onto the Files tab, or use the picker. Show a staged list with sizes and paths (editable target folder), commit message, branch (or **Open as pull request**).
- Compute SHA-256 in a Web Worker per file (streamed, with progress), then call the native upload API, which runs the same preupload and LFS steps as the Hub API ([03](03-hf-api.md)). Upload parts in parallel (default 4) straight to presigned S3 part URLs when available, else to Hangar. Resume after a page reload by re-hashing and asking which parts exist.
- Small text files go inline in the Commit.
- Warn before leaving the page while an upload runs.

## States

Every data view has loading (skeletons), empty (copy from [01](01-product.md)), error (problem detail message and retry) and permission-denied states. Mirrors show live Pull progress on the Repo page and in the Jobs drawer, polling every 2 seconds while a Job runs.
