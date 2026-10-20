---
description: "Use Hangar from Python: point huggingface_hub, transformers, sentence-transformers and diffusers at your Instance with HF_ENDPOINT."
---

# Use Hangar from Python

Every library built on `huggingface_hub` works against Hangar once `HF_ENDPOINT` points at it:

```sh
export HF_ENDPOINT=https://hangar.example.com
export HF_TOKEN=hgr_...
export DISABLE_SAFETENSORS_CONVERSION=1
```

(`eval "$(hangar env)"` sets all three.) The compatibility suite runs `huggingface_hub`, `transformers`, `sentence-transformers`, `datasets` and `diffusers` against Hangar on every change; the [compatibility matrix](../reference/hub-api-compatibility.md) lists every Hub endpoint and its status.

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

A file of a Partial mirror that was not pulled is listed with its size, but downloading it fails with HTTP 409 (`HfHubHTTPError` in `huggingface_hub`), and the message names the `hangar pull` command to run. If the admin turned on Pull-through, the download starts the file's Pull instead and streams it as it arrives. Pull it first with `hangar pull <repo> --include <file>` or from the Repo page.

## Large downloads

On S3 storage in `redirect` mode, downloads go straight to presigned URLs, so file bytes do not pass through Hangar. `huggingface_hub` refuses plain HTTP downloads over 50 GB when it knows the size; Hangar serves files over 50 GB in `redirect` mode in a way the client accepts. In `proxy` mode and on the filesystem Blob store, download files over 50 GB with `hangar pull` or `git lfs` instead.
