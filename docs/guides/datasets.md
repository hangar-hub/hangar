# Work with datasets

Datasets are Repos of type `dataset`. In URLs and the CLI they carry a `datasets/` prefix: `datasets/acme/reviews`.

## In the web UI

Datasets have their own section, **Datasets** in the top bar (`/datasets`), separate from **Models**: each list holds one type only, with its own filters and its own **New dataset** button. Old `/arsenal` links still work. They open the Models list, or the Datasets list for `?type=datasets`.

A dataset can hold any files, not only training data: for example, sample inputs for another project's tests. When none of its files is a data file the Dataset Viewer reads (Parquet, CSV, JSON Lines, text, Arrow, WebDataset archives, or folders of images or audio), the **Viewer** tab says there is nothing to preview and links to the files, and **Use this dataset** shows the snippets that download the files: the `hf` CLI, `huggingface_hub` and git.

## Link models and datasets

Links are optional. A model names the datasets it was trained or evaluated on in its Model card's `datasets` metadata, as on the Hugging Face Hub:

```yaml
---
datasets:
  - acme/reviews
  - imdb
---
```

To set it in the UI, open the model's **Settings** tab and use **Datasets used**: search the arsenal by name, or type the ID of a dataset on an Upstream, then **Commit to the Model card**. Hangar keeps the card's other metadata and text. The model's sidebar then lists the datasets under **Datasets used**, and each dataset's sidebar lists the model under **Used by**. Mirrors keep their upstream card, so set it on the Upstream instead.

To group a project's models and datasets, put them in a [Collection](collections.md).

## Load them

With `HF_ENDPOINT` pointing at Hangar:

```python
from datasets import load_dataset

ds = load_dataset("lhoestq/demo1", split="train")
stream = load_dataset("lhoestq/demo1", split="train", streaming=True)
```

pandas reads the `hf://` file system that `huggingface_hub` provides:

```python
import pandas as pd

df = pd.read_parquet("hf://datasets/acme/reviews/data/train-00000-of-00001.parquet")
```

Polars has its own `hf://` client, which always talks to huggingface.co and ignores `HF_ENDPOINT`. Open the files through `huggingface_hub` instead:

```python
import polars as pl
from huggingface_hub import HfFileSystem

fs = HfFileSystem()
paths = fs.glob("datasets/acme/reviews/**/*.parquet")
df = pl.concat([pl.read_parquet(fs.open(p, "rb")) for p in paths])
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

A Mirror keeps the Upstream's own Parquet export instead of converting. A Pull of the dataset's files does not always bring the export's files with it; until they are in the Arsenal, the **Viewer** tab says to pull them, which `hangar pull datasets/<namespace>/<name> --parquet-only --server-only` does.

## Partial and Parquet-only Pulls

Pull only the Configs and Splits you need, or only their Parquet export, which can turn a very large raw dataset into a few GB:

```sh
hangar pull datasets/HuggingFaceFW/fineweb-edu --config sample-10BT --split train --parquet-only --to ./fineweb
```

In the UI, open a dataset in **Discover** and choose Configs, Splits and **Parquet export only** before **Pull into arsenal**.

## The Dataset Viewer

The **Viewer** tab of a dataset previews rows, searches and filters them, and shows statistics for each column, through an API compatible with the Hub's Dataset Viewer at `<public_url>/datasets-server/...`. Until a Split is converted, it shows "Preparing preview".
