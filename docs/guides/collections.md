# Collections

A Collection is a named, ordered list of Repos that a User or Organisation curates, such as "Embedding models we support" or "Datasets for the March evaluation". Each item can have a note.

Create one from **Collections** in the UI, or from a Repo page with **Add to collection**. Collections have their own visibility, like Repos.

The Hub collections API works against Hangar, so `huggingface_hub` can manage them:

```python
from huggingface_hub import HfApi

api = HfApi()  # HF_ENDPOINT points at Hangar
c = api.create_collection("Embedding models", namespace="acme", private=True)
api.add_collection_item(c.slug, item_id="sentence-transformers/all-MiniLM-L6-v2", item_type="model", note="Default")
```
