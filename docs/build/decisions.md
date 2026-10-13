# Phase 0 decisions and open questions

Where the spec was silent or inconsistent, phase 0 chose as follows. Change any of these through `requests/` with a reason.

## Decisions

- **Go 1.27.2.** The current stable release on 2026-10-13, pinned with the `toolchain` directive.
- **Node 24 LTS and pnpm 11.28.2.** Node 20 reached end of life in April 2026 and Vite 8 needs 20.19 or later. pnpm 12 does not run through the installed corepack, so the UI pins the latest pnpm 11. TypeScript stays on 5.9 because `openapi-typescript` 7 needs TypeScript 5.
- **Column `commit_sha`, not `commit`.** `COMMIT` is a keyword in SQLite, so the tables in 02 that have a `commit` column (`tree_entries`, `dependencies`, `metadata`, `scan_results`, `dataset_splits`, `dataset_stats`) use `commit_sha`. `dataset_stats.column` is `column_name` for the same reason.
- **Column `tokens.scope`, not `scopes`.** A Token has one scope level (05-auth.md: admin includes write, write includes read).
- **Extra tables.** `ollama_blobs` (08), plus `ollama_nonces`, `invites`, `password_resets`, `login_attempts`, `oidc_states`, `oauth_device_codes` and `rate_limit_leases`, because 05, 06 and 08 need state that must be shared across replicas.
- **Extra configuration keys.** `server.data_dir` (where `secrets.yaml` and the SQLite lock live), `server.shutdown_timeout` and `server.timezone` (named in 02 and 11), `repos.restore_window` (REPO-5), `hfapi.default_namespace` (single-segment redirects), `upstreams.<id>.namespace_prefix` and `viewer_url` (06, 14), `pull.pull_through.min_role` (06), `viewer.*` (14) and `telemetry.environment`, `prometheus_listen`, `log_format`, `log_level` (11). `git.cache_dir`, `git.cache_size` and `git.timeout` follow the YAML in 02, not the `server.git_*` names in 04.
- **Telemetry defaults.** 02 shows `otlp` and 11 says a quickstart makes no connections. Both hold: the default is `otlp`, which exports only when an OTLP endpoint variable is set.
- **Flags.** Every key can be set with `--set key=value`; `--role`, `--listen` and `--public-url` are shortcuts.
- **Reserved names.** `new-dataset` is reserved as well as the list in 02, because it is a UI route at the root.
- **Default `.gitattributes`.** Copied from Repos created on huggingface.co on 2026-10-13: one file for models and one for datasets, which the Hub gives extra media rules.
- **Native API.** The Discover endpoints take the Repo type in the path (`/discover/{type}`), so datasets use the same shape as models (DATA-7). `GET /repos/datasets/{ns}/{name}/configs` gives Configs, Splits and conversion status for the Viewer tab, owned by W10.
- **Phase 0 server wiring.** `cmd/hangar` serves the real health checks, the embedded UI and the generated native API router (answering 501 until handlers exist). The other protocols are stand-ins from `internal/httpx/httpxtest`, the Blob store is in memory, and every request is anonymous. Phase 2 replaces them.

## Open questions for James

- The default bootstrap username `admin` is also a reserved Namespace name, and `/admin/...` is a UI route, so the admin's personal Namespace page and any Repos in it would be shadowed by the admin UI. Config validation allows the default; W5 must either keep the bootstrap user from owning Repos or change the default (for example to `hangar-admin`).
- Native API endpoints with no owner in the build plan: `/search`, `/jobs`, `/namespaces/{ns}`, `/admin/settings`, `/admin/system`, `/me/device`. Suggested owners: `/jobs` W1, `/search` and `/namespaces` W2, `/me/device` W5, `/admin/settings` and `/admin/system` the integration lead.
- `internal/contracttest`, `internal/secrets`, `internal/pins` and `internal/httpx/httpxtest` are new packages not in the layout in 02. The integration lead owns them.
