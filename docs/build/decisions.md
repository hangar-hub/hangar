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
- **Bootstrap admin username `hangar-admin`.** Decided by the integration lead on 2026-10-13. The default `security.bootstrap_admin.username` is `hangar-admin`, not `admin`, because `admin` is a reserved Namespace name and `/admin` is a UI route, so the bootstrap admin's personal Namespace page and Repos would be shadowed by the admin UI. Config validation refuses reserved names for this key. W5 implements the bootstrap step.
- **Owners of native API endpoints the build plan left unowned.** Decided by the integration lead on 2026-10-13: `/jobs` and `/admin/system` belong to W1, `/search` and `/namespaces` to W2, and `/me/device` and `/admin/settings` to W5.
- **Packages outside the layout in 02.** Decided by the integration lead on 2026-10-13: the integration lead owns `internal/contracttest`, `internal/secrets`, `internal/pins` and `internal/httpx/httpxtest`. Phase 2 deleted `internal/pins` once every pinned module was imported by real code.

## Decisions from the bellatrix preview

James used a preview on his home cluster on 2026-10-16 and decided the following that day.

- **Taxiway teal accent.** Decided by James on 2026-10-16. The UI's accent changes from amber to Taxiway teal: light `--hg-accent` `#0b6e64`, `--hg-accent-fill` `#0b7a6e`, `--hg-accent-ink` `#ffffff`, `--hg-accent-soft` `#dff3ef`; dark `#5fd4c2`, `#2bb3a0`, `#021a17`, `#12332e`. Warnings stay amber, so the accent no longer looks like a warning. The amber colour tokens are gone, components use only the semantic tokens, and the logo's bay line, the favicon, the docs icon and the docs site accent follow. The palette is in [09](../spec/09-web-ui.md) "Visual direction".
- **Models and Datasets are separate sections.** Decided by James on 2026-10-16. The top bar has **Models** (`/models`) and **Datasets** (`/datasets`) in place of the mixed **Arsenal** list. Each has its own list, filters, empty state and **New model** or **New dataset** button, and no list mixes the two types: the search palette groups results by type and Namespace pages list models and datasets separately. `/` shows the Models list to signed-in users. `/arsenal` stays as a redirect so old links work: to `/datasets` for `type=datasets`, to `/models` otherwise, with the other query parameters kept. Collections stay the way to group a project's models and datasets. "Arsenal" still names everything in an Instance (`CONTEXT.md`).
- **Optional links between models and datasets.** Decided by James on 2026-10-16. A model's **Settings** tab has a **Datasets used** picker that commits the Model card's `datasets` metadata as the Hub stores it, a list of dataset IDs, keeping the card's other keys and text. The model's sidebar lists them under **Datasets used** and each dataset's sidebar lists the models under **Used by**, from the Dependencies Hangar already resolves from the card. A dataset with no links is normal.
- **Datasets hold any files.** Decided by James on 2026-10-16. A dataset of files the Dataset Viewer cannot read, such as sample data for another project, reads as a normal dataset: the Viewer tab says there is nothing to preview and why, and links to the files; **Use this dataset** offers only the download snippets.
- **Equal-height cards.** Decided by James on 2026-10-16. Repo cards in a grid row are the same height, with the stats row at the bottom, and show three quant chips, then a **+N** chip whose accessible name and tooltip list the rest.

## Open questions for James

None.
