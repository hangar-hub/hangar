# 14. Datasets

Datasets are first-class in Hangar, not just a Repo type. You can discover datasets on an Upstream, pull a whole dataset or just one Config and Split, upload your own, preview and query rows in the browser, and load them with the `datasets` library, pandas or Polars through the same `HF_ENDPOINT` swap as models, or with DuckDB over HTTPS `resolve` URLs. (Whether DuckDB's own `hf://` support honours a custom endpoint is unverified; W10 checks the current DuckDB httpfs source and documents the answer.)

## Concepts

These are added to `CONTEXT.md`.

- **Config**: a named subset of a dataset (the Hub also calls it a "subset" or "configuration"), declared in the dataset card's `configs:` YAML or inferred from the file layout. The default is `default`.
- **Split**: a named part of a Config, such as `train`, `validation` or `test`.
- **Parquet export**: Parquet shards of every Config and Split, kept on the `refs/convert/parquet` branch, as on the Hub. The Dataset Viewer and fast previews read from it.
- **Dataset Viewer**: the row preview, search, filter and statistics service, with an API compatible with the Hub's Dataset Viewer (`datasets-server`).

## Features (DATA)

- **DATA-1** Dataset Repos have the same versioning, permissions, uploads, git and Hub API support as model Repos (already REPO-1 and onwards). This group adds what is specific to datasets.
- **DATA-2** Config and Split resolution: read the card's `configs:` YAML (`data_files`, `data_dir`, `default`, per-config builder options such as CSV `sep`), else infer Splits from file and directory names using the same default patterns as the `datasets` library (port `datasets/data_files.py` split patterns and keyword lists, with tests against its own examples).
- **DATA-3** Parquet export: a `dataset.convert` Job writes Parquet shards to `refs/convert/parquet` in the Hub's layout (`{config}/{split}/0000.parquet`, ...; `{config}/partial-{split}/...` when a Split is larger than `viewer.max_convert_bytes`, default 5 GiB, matching the Hub). Source formats: Parquet (re-sharded only if needed), CSV and TSV, JSON and JSON Lines, plain text (one row per line), Arrow IPC, WebDataset tar shards, and ImageFolder and AudioFolder layouts (with `metadata.csv` or `metadata.jsonl`), storing media as bytes the way the Hub does. Features (the Hub's `Features` JSON: `Value`, `ClassLabel`, `Sequence`, `List`, `Image`, `Audio`, nested structs) are inferred, or taken from the card's `dataset_info`.
- **DATA-4** Dataset Viewer API, compatible with the Hub's, for `/is-valid`, `/splits`, `/first-rows`, `/rows`, `/search`, `/filter`, `/parquet`, `/info`, `/size` and `/statistics`. Served at `<public_url>/datasets-server/...` and, when a request's `Host` is `datasets-server.<public host>`, at the root. Admins who create that DNS name get full compatibility with tools that derive the viewer host the way `datasets` does.
- **DATA-5** Statistics per column, computed during conversion and matching the Hub's `/statistics` shape: null counts and proportions, min, max, mean, median and standard deviation, histograms for numbers and string lengths, frequencies for class labels and low-cardinality strings, and image and audio dimensions and durations.
- **DATA-6** Viewer tab in the UI: Config and Split pickers, a virtualised table with paging (100 rows per page), cell rendering for text (expandable), numbers, class labels (name and id), lists and structs (collapsible JSON), images (thumbnails, opening full size) and audio (player); full-text search box; a filter bar that builds a `where` clause (column, operator, value) with a raw SQL toggle; sort by column; column header statistics (histogram or top values on hover); row count, size and Parquet export status.
- **DATA-7** Discover for datasets: search Upstream datasets with Hub facets (`modality:`, `format:`, `task_categories:`, `language:`, `size_categories:`, licence, sort), with arsenal state badges. The detail panel shows the card, Configs and Splits with row counts and sizes, and a preview of the first rows, using the Upstream's own Dataset Viewer API (`<upstream>/` with `://` replaced by `://datasets-server.`, overridable per Upstream with `viewer_url`).
- **DATA-8** Partial dataset Pulls: select Configs and Splits (translated to the file globs from DATA-2 at the chosen Revision), or **Parquet export only** for selected Configs and Splits (pulls those files from the Upstream's `refs/convert/parquet`, which Mirrors fetch anyway, so a 500 GB raw dataset can arrive as a few GB of Parquet). Selections are stored by Config and Split name, so tracking follows new files.
- **DATA-9** Dataset metadata in the index ([07](07-indexing-and-scanning.md)): Configs, Splits, rows and bytes per Split, features, modalities, formats, task categories, languages, size category (computed from rows, using the Hub's buckets such as `1K<n<10K`), licence, pretty name, and the models whose cards list this dataset (and the reverse on model pages).
- **DATA-10** Croissant metadata: `GET /api/datasets/{repo}/croissant` returns MLCommons Croissant JSON-LD built from the card, Configs, Splits and features, as the Hub does.
- **DATA-11** Arsenal facets for datasets: modality, format, task, language, size category, rows, licence, Mirror or local.
- **DATA-12** CLI: `hangar pull datasets/<ns>/<name> [--config ...] [--split ...] [--parquet-only] [--to DIR]` and `hangar push --type dataset`. `hangar info datasets/...` shows Configs, Splits, rows and sizes. Repo IDs with a `datasets/` prefix are accepted everywhere the CLI takes a Repo ID.
- **DATA-13** Dataset cards: the UI renders the card, shows the YAML metadata in a sidebar, and offers a card editor with a form for the common fields (pretty name, licence, task categories, languages, Configs) that writes valid YAML.

## Dataset Viewer details

### Data source

The viewer reads only the Parquet export. Until a Split is converted, `/is-valid` reports it as not ready, the UI shows "Preparing preview" with Job progress, and `/first-rows` falls back to reading the first rows from the source file directly when the format allows (Parquet, CSV, JSON Lines, text).

Readers use `github.com/parquet-go/parquet-go` (Apache-2.0) over ranged Blob store reads: footers first, then only the row groups and columns needed. Row-group statistics prune `/filter` scans.

### Query semantics

- `/rows`: `offset` and `length` (maximum 100), exact rows by index across shards using per-shard row counts.
- `/filter`:
  - `where`: comparison predicates on double-quoted columns with `=`, `<>`, `!=`, `<`, `<=`, `>`, `>=`, `IS [NOT] NULL`, `IN (...)`, `LIKE` and `ILIKE`; string and numeric literals; `AND`, `OR`, `NOT` and parentheses.
  - `orderby`: one or more columns, each with optional `ASC` or `DESC`.
  - Parsed by a small, fuzz-tested parser in `internal/datasets/query`. Never pass the input to a SQL engine.
- `/search`: case-insensitive match of every query term in any string column (the Hub uses full-text search; Hangar's results are a superset, which is acceptable and documented).
- Scans stop after `viewer.max_scan_rows` (default 5 million) or `viewer.max_scan_seconds` (default 10). The response then sets `partial: true` and `num_rows_total` to the number counted so far. Results for a `(commit, config, split, where, orderby)` are cached in the Blob store as a row-index list, so paging through them is fast.
- Responses use the Hub's shapes: `features[{feature_idx, name, type}]`, `rows[{row_idx, row, truncated_cells}]`, `num_rows_total`, `num_rows_per_page`, `partial`. Cells over `viewer.max_cell_bytes` (default 100 KiB) are truncated and listed in `truncated_cells`. Images and audio are returned as URLs to `/datasets-server/assets/...`, signed and short-lived, as the Hub does.
- `/opt-in-out-urls` and `/presidio-entities` return valid empty results.

### Access

Viewer endpoints apply the Repo's read permissions. They accept the same Bearer tokens and the UI session cookie. Private datasets return the Hub's viewer error shape with status 404 to callers who cannot read them.

## Architecture additions

- Package `internal/datasets` with `layout` (Config and Split resolution), `convert` (readers per format, Parquet writer, features inference), `stats`, `query` (where and orderby parser and evaluator), `viewer` (HTTP handlers) and `croissant`.
- Tables:
  - `dataset_splits(repo_id, commit, config, split, num_rows, num_bytes_source, num_bytes_parquet, features (json), status (pending, ready, partial, failed), error, converted_commit)`
  - `dataset_stats(repo_id, commit, config, split, column, stats (json))`
- Jobs: `dataset.convert` (per Repo Commit, after `index`, on default-branch heads only, low priority, bandwidth-free because it reads local data) and `dataset.stats` (after convert).
- Commits to `refs/convert/parquet` are made by the system user `hangar-bot` through `repo.Service`. Writes to that ref by anyone else are refused.
- Routing additions in [02](02-architecture.md): `/datasets-server/...`, Host-based routing for `datasets-server.<host>`, and `datasets-server` as a reserved Namespace name.
- Config block:

```yaml
viewer:
  enabled: true
  max_convert_bytes: 5GiB        # per Split; larger Splits get a partial export
  max_scan_rows: 5000000
  max_scan_seconds: 10
  max_cell_bytes: 100KiB
  convert_concurrency: 1         # dataset.convert Jobs per worker
```

## Hub API additions ([03](03-hf-api.md))

- `GET /api/datasets/{repo}/parquet` and `/parquet/{config}/{split}`: lists of Parquet export URLs (Hub shape).
- `GET /api/datasets/{repo}/croissant`.
- `refs` includes `converts` (`refs/convert/parquet`) once an export exists.
- Rule 15 in [03](03-hf-api.md) changes: `datasets-server.<host>` must either not resolve, or point at Hangar. Hangar answers `/info` within 1 second from the `dataset_splits` table, or with the Hub's "not ready" error.

## Tests ([13](13-testing.md))

- `compat/hf`: `load_dataset` of fixture datasets in every source format (Parquet, CSV, JSON Lines, text, ImageFolder with `metadata.csv`, AudioFolder), streaming and non-streaming, by Config and Split; pandas `read_parquet("hf://datasets/...")` and Polars `scan_parquet("hf://...")` against Hangar.
- `compat/viewer`: for 6 small public datasets, convert them in Hangar and compare `/splits`, `/info`, `/first-rows`, `/rows`, `/size` and `/statistics` with the Hub's responses for the same Revision (exact for structure, counts and row content; within 1 % for floating-point statistics). Record any intentional differences in `compat/viewer/DIFFERENCES.md`.
- Fuzz targets for the `where` and `orderby` parser, CSV and JSON Lines readers, and features inference.
- Load: `/rows` p99 under 200 ms for a 10 million row Split; `/filter` on a 1 million row Split completes within `max_scan_seconds`.
