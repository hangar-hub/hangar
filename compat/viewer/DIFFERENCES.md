# Dataset Viewer differences from the Hub

Hangar's Dataset Viewer (`/datasets-server/...`) answers with the Hub's response shapes, error codes and values. The `compat/viewer` suite compares both for the same Revision of seven public datasets, with exact structure, counts and row content and floats within 1 % in `/statistics`. This page lists where Hangar differs on purpose, and where the Hub's recorded responses differ from the Hub's current code. The suite normalises or allows each of these, and nothing else.

Run the suite with `go test -tags compat ./compat/viewer`. It records the Hub's responses with `compat/viewer/snapshot.py` the first time.

## Intentional differences

### Media URLs

The Hub signs `src` URLs for CloudFront (`cached-assets/...?Expires=...&Signature=...&Key-Pair-Id=...`). Hangar signs its own URLs under `/datasets-server/assets/{namespace}/{name}/--/{commit}/--/{config}/{split}/{row}/{column}/{file}` with the Instance key, valid for `AssetTTL` (one hour by default). A media cell inside a list or struct adds `jp`, the cell's JSON path. File names follow the Hub, including the hash suffix of nested cells (`image-1d100e9.png`).

The suite drops `src` before comparing; `height`, `width` and `type` must match.

### Media bytes

The Hub re-encodes images and audio with Pillow and soundfile before it serves them. Hangar serves the bytes stored in the Parquet export unchanged, with `Content-Security-Policy: default-src 'none'; sandbox` and `X-Content-Type-Options: nosniff`. Dimensions and MIME types are the same; file sizes can differ.

### Parquet byte sizes of converted datasets

Hangar writes the Parquet export with its own writer (parquet-go, Snappy, the Hub's row group sizes of 1,000 rows, or 100 for media). The rows, Features and `huggingface` metadata are the Hub's, but the files are not byte-identical, so `size` in `/parquet` and `num_bytes_parquet_files` in `/size` differ for converted datasets. They are equal for Parquet datasets, whose files both copy. The suite compares these sizes only for copied datasets.

Hangar's files carry no `ARROW:schema` metadata. `datasets` restores every type from the `huggingface` Features, but plain pyarrow and pandas readers see a `timestamp[s]` column as `timestamp[ms]`, since Parquet has no seconds unit.

### Private and missing datasets

For a dataset the caller cannot read, the Hub answers 401 or 404 with `ExternalUnauthenticatedError` (anonymous) or `ExternalAuthenticatedError`. Hangar always answers 404, with the same codes and messages, so a response never confirms that a private dataset exists (docs/spec/14-datasets.md).

### /search

The Hub ranks matches with DuckDB full-text search (BM25 over stemmed words). Hangar matches rows where every term of the query appears, case-insensitively, in some string column, and returns them in dataset order. Hangar can return rows the Hub's stemming misses and the other way round, and the order differs. `/filter` evaluates the same `where` and `orderby` grammar as the Hub, so its results match.

### Scanning endpoints

`/opt-in-out-urls` and `/presidio-entities` return the Hub's empty result: Hangar does not scan for opted-out image URLs or personal data.

### Statistics the Hub cannot compute

- An integer column whose values span exactly two numbers, such as 2 and 3, fails on the Hub (`Got unexpected result during histogram computation`). Hangar answers with one bin, `[2, 3]`, holding every value.
- A float column of one value has no sample standard deviation: Hangar answers `null`, where the Hub fails.
- A float column whose range includes an infinity fails on both, as `np.arange` cannot make the bins (`arange: cannot compute length`).

### /info and /size of every conversion

Hangar fills `download_checksums`, `download_size` and `size_in_bytes` in `/info`, and `num_bytes_original_files` in `/size`, for every converted Config. `download_checksums` keys name Hangar's `hf://datasets/{namespace}/{name}@{commit}/{path}`. The Hub leaves some of these out for datasets its older workers converted. The suite compares them when the Hub sends them, with the Repo name and Revision in the keys normalised.

### Croissant

`/api/datasets/{repo}/croissant` follows the Hub's `croissant_utils.py`: the same record sets, field IDs, data types and JSON paths. Differences:

- The description is the card's Markdown as plain text. The Hub renders it as HTML first, so whitespace differs.
- `keywords` are the card's task categories and languages, the size category, the formats and the modalities. The Hub adds library names and the hosting region.
- The FileSet describes "the underlying Parquet files as converted by Hangar", and `contentUrl` is the Instance's `tree/refs%2Fconvert%2Fparquet` page.
- `creator` is the Namespace, without a display name.

### Card metadata edge cases

- ClassLabel names written as numbers in YAML (`'0': 1`) are kept as the strings `"1"`, as `ClassLabel` uses them. The Hub's `/info` shows them as numbers.
- When a YAML mapping lists a key before a `<<` merge key, Hangar keeps that key first. PyYAML puts merged keys first. The values are the same.

## Responses cached by older Hub workers

The Hub keeps a cached response until the dataset changes, so a recorded response can predate the Hub's current code. Hangar follows the current code. For each case below, the Hub's current code (dataset-viewer `main` at 2026-10-13) or `datasets` 5.1.0 gives Hangar's answer. The suite logs these differences instead of failing (`knownDifferences` in `viewer_test.go`).

| Dataset | Response | Hub (cached) | Current code and Hangar |
|---|---|---|---|
| `lhoestq/demo1` | `/statistics`, integer histograms | An extra closing bin, `bin_edges` `[1, 2, 3, 4, 5, 5]` | `[1, 2, 3, 4, 5]` (`generate_bins` in `statistics_utils.py`) |
| `cornell-movie-review-data/rotten_tomatoes` | `num_bytes` in `/info` and `num_bytes_memory` in `/size` | 1,072,741 for `train` | 1,020,602, the row groups' `total_byte_size` (`fill_builder_info`) |
| `datasets-examples/doc-audio-6` | `num_bytes` in `/info` and `/size` | 324,262, from an older Revision | 324,026, as `datasets` 5.1.0 counts these files |
| `lhoestq/demo1` | Croissant `dataType` of `int64` columns | `sc:Integer` | `cr:Int64` (`HF_TO_CROISSANT_VALUE_TYPE`) |

## Client notes

### DuckDB `hf://` paths

DuckDB's `httpfs` extension hard-codes `https://huggingface.co` as the endpoint of `hf://` paths (`src/include/hffs.hpp`, checked at commit `459b80884a9aa6e08d4680481c9d4e4751c12dc0`) and ignores `HF_ENDPOINT`. DuckDB can still read Hangar's Parquet export over HTTPS:

```sql
SELECT * FROM 'https://hangar.example.com/datasets/ns/name/resolve/refs%2Fconvert%2Fparquet/default/train/0000.parquet';
```

For a private dataset, pass a token with `CREATE SECRET (TYPE http, BEARER_TOKEN 'hf_...')`.
