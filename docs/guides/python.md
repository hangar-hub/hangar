---
title: transformers and huggingface_hub on a private hub
description: "Use Hangar from Python: point huggingface_hub, transformers, sentence-transformers and diffusers at your Instance with HF_ENDPOINT."
---

# Use Hangar from Python

Every library built on `huggingface_hub` works against Hangar once `HF_ENDPOINT` points at it:

```sh
export HF_ENDPOINT=https://hangar.example.com
export HF_TOKEN=hgr_...
export DISABLE_SAFETENSORS_CONVERSION=1
```

(`eval "$(hangar env)"` sets all three.) The compatibility suite runs `huggingface_hub`, `transformers`, `sentence-transformers` and `datasets` against Hangar on every pull request, and `diffusers` nightly. [Tested versions and platforms](../reference/compatibility.md) lists the versions, and the [compatibility matrix](../reference/hub-api-compatibility.md) lists every Hub endpoint and its status.

## huggingface_hub

```python
from huggingface_hub import HfApi, hf_hub_download, snapshot_download

api = HfApi()
info = api.model_info("bartowski/Llama-3.2-3B-Instruct-GGUF")
path = hf_hub_download("hf-internal-testing/tiny-random-bert", "config.json")
folder = snapshot_download("hf-internal-testing/tiny-random-bert", allow_patterns=["*.json", "*.safetensors"])
```

Revisions work as on the Hub: a branch, a tag, a Commit SHA or `refs/pr/1`. Downloads resume after an interruption, and with `HF_HUB_OFFLINE=1` everything already in the cache loads with no network.

## transformers and sentence-transformers

```python
from transformers import AutoModel, AutoTokenizer
from sentence_transformers import SentenceTransformer

model = AutoModel.from_pretrained("hf-internal-testing/tiny-random-bert")
tokenizer = AutoTokenizer.from_pretrained("hf-internal-testing/tiny-random-bert")
encoder = SentenceTransformer("sentence-transformers/all-MiniLM-L6-v2")
```

Models with remote code (`trust_remote_code=True`) load offline as soon as their Pull finishes, because Hangar pulls the code Dependencies they name in `auto_map` first.

## diffusers

```python
from diffusers import DiffusionPipeline

pipe = DiffusionPipeline.from_pretrained("hf-internal-testing/tiny-stable-diffusion-torch")
```

## Files that are not in the Arsenal

A Partial mirror lists every file of the Repo with its size, but holds only the files that were pulled. Downloading a file it does not hold fails with HTTP 409 and the error code `EntryNotFound`, which `huggingface_hub` raises as `RemoteEntryNotFoundError`. The message names the `hangar pull` command that gets the file.

`snapshot_download` stops at the first such file, so pass `allow_patterns` to download only the files the mirror holds:

```python
from huggingface_hub import snapshot_download

folder = snapshot_download("bartowski/Llama-3.2-1B-Instruct-GGUF", allow_patterns=["*Q4_K_M.gguf", "*.json", "*.md"])
```

With the `hf` CLI, use `--include` in the same way. To get a missing file, pull it with `hangar pull <repo> --include <file>` or from the Repo page.

If the admin turned on Pull-through, a download of a file that is not held starts the file's Pull instead and streams the file as it arrives. Ranged requests, such as a resumed download, work too: they wait until the transfer reaches the start of the range.

## Large downloads

On S3 storage in `redirect` mode, downloads go straight to presigned URLs, so file bytes do not pass through Hangar. `huggingface_hub` refuses plain HTTP downloads over 50 GB when it knows the size; Hangar serves files over 50 GB in `redirect` mode in a way the client accepts. In `proxy` mode and on the filesystem Blob store, download files over 50 GB with `hangar pull` or `git lfs` instead.
