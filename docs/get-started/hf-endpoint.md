# Point HF_ENDPOINT at Hangar

Hugging Face libraries read the Hub's address from `HF_ENDPOINT`, and a Token from `HF_TOKEN`. Set both and they use Hangar:

```sh
export HF_ENDPOINT=https://hangar.example.com
export HF_TOKEN=hgr_...
export DISABLE_SAFETENSORS_CONVERSION=1
```

`hangar env` prints these for the Instance you signed in to:

```sh
eval "$(hangar env)"
```

`DISABLE_SAFETENSORS_CONVERSION=1` matters for models that only have pickle weights (`pytorch_model.bin`). Without it, `transformers` may send the Repo ID, and a Token you passed to it, to a conversion service on huggingface.co. Hangar also refuses the request `transformers` makes before that call, so the leak cannot happen, but setting the variable avoids the attempt.

Now every Hub call goes to Hangar:

```python
from transformers import AutoModel, AutoTokenizer

model = AutoModel.from_pretrained("hf-internal-testing/tiny-random-bert")
tokenizer = AutoTokenizer.from_pretrained("hf-internal-testing/tiny-random-bert")
```

Files land in the usual cache, `~/.cache/huggingface/hub`, which the `hangar` CLI shares.

## In Python only

```python
import os
os.environ["HF_ENDPOINT"] = "https://hangar.example.com"   # before importing huggingface_hub
```

or pass `endpoint=` to `HfApi`, `hf_hub_download` and `snapshot_download`.

## Datasets

`datasets` also calls `datasets-server.<host>`. Point that name at Hangar too, which serves the Dataset Viewer API there; see [Datasets](../guides/datasets.md#the-datasets-server-name).

More: [Python libraries](../guides/python.md).
