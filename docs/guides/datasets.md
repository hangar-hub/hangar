# Work with datasets

Datasets are Repos of type `dataset`. In URLs and the CLI they carry a `datasets/` prefix: `datasets/acme/reviews`.

## Load them

With `HF_ENDPOINT` pointing at Hangar:

```python
from datasets import load_dataset

ds = load_dataset("lhoestq/demo1", split="train")
stream = load_dataset("lhoestq/demo1", split="train", streaming=True)
```

pandas and Polars read the `hf://` file system that `huggingface_hub` provides:

```python
import pandas as pd
import polars as pl

df = pd.read_parquet("hf://datasets/acme/reviews/data/train-00000-of-00001.parquet")
lf = pl.scan_parquet("hf://datasets/acme/reviews/**/*.parquet")
```

DuckDB reads files over HTTPS from `resolve` URLs. Its own `hf://` scheme talks to huggingface.co, so use HTTPS URLs for Hangar:

```sql
INSTALL httpfs; LOAD httpfs;
CREATE SECRET hangar (TYPE http, EXTRA_HTTP_HEADERS MAP {'Authorization': 'Bearer hgr_...'});
SELECT count(*) FROM 'https://hangar.example.com/datasets/acme/reviews/resolve/refs%2Fconvert%2Fparquet/default/train/0000.parquet';
```

## The datasets-server name

`datasets` asks `datasets-server.<your host>/info` about a dataset before loading it, with a long timeout. Hangar serves the Dataset Viewer API there, so create a DNS name `datasets-server.hangar.example.com` that points at Hangar (the Helm chart's Ingress adds it with `ingress.datasetsServerHost: true`), or make sure the name does not resolve. Without either, `load_dataset` can wait a long time before it starts.

## Configs and Splits

A Config is a named subset of a dataset, from the card's `configs:` YAML or inferred from the file layout, and a Split is a part of a Config, such as `train` or `test`. `hangar info datasets/acme/reviews` lists them with row counts and sizes.

## Parquet export

Hangar converts every Config and Split to Parquet, on the `refs/convert/parquet` branch, as huggingface.co does. The Dataset Viewer reads it, and you can load it directly.

## Partial and Parquet-only Pulls

Pull just the Configs and Splits you need, or only their Parquet export, which can turn a very large raw dataset into a few GB:

```sh
hangar pull datasets/HuggingFaceFW/fineweb-edu --config sample-10BT --split train --parquet-only --to ./fineweb
```

In the UI, open a dataset in **Discover** and choose Configs, Splits and **Parquet export only** before **Pull into arsenal**.

## The Dataset Viewer

The **Viewer** tab of a dataset previews rows, searches and filters them, and shows statistics for each column, through an API compatible with the Hub's Dataset Viewer at `<public_url>/datasets-server/...`. Until a Split is converted, it shows "Preparing preview".
