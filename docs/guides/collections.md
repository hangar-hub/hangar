---
title: Hugging Face Collections on a self-hosted hub
description: "Curate Collections in Hangar: named, ordered lists of models, datasets and other Collections, with a note on each, from the web UI, huggingface_hub or the API."
---

# Collections

A Collection is a named, ordered list of Repos that a User or Organisation curates, such as "Embedding models we support" or "Datasets for the March evaluation". Each item can have a note. A Collection can hold models, datasets and other Collections.

## Who can see and change a Collection

A Collection has its own visibility, which works like a Repo's:

| Visibility | Who can see it |
|---|---|
| `public` (the default) | Anyone signed in, and anonymous visitors if the Instance allows anonymous reads (`security.anonymous_read`). |
| `internal` | Any signed-in User. |
| `private` | Members of the Namespace that owns it, and Instance admins. |

Each viewer sees only the items they can read: a private Repo in a public Collection stays hidden from people who cannot read the Repo.

To create or change a Collection in a Namespace, you need the `write` Role in it. Your personal Namespace is always yours, and Instance admins can change any Collection.

## In the web UI

1. Open **Collections** and select **New Collection**. Choose the Namespace, a title and the visibility.
2. On the Collection's page, select **Edit**. Add a Repo by its ID, such as `acme/bert-tiny`, or `datasets/acme/reviews` for a dataset.
3. Drag items by their handle to reorder them, or focus a handle and use the arrow keys, then press Space. Add a note to an item, and select **Save note**.
4. Select **Done**.

A Repo's page lists the Collections it is in under **In Collections**. Deleting a Collection does not affect the Repos in it.

## With huggingface_hub

The Hub's collections API works against Hangar, so `huggingface_hub` can manage Collections. The compatibility suite tests each of these calls:

```python
from huggingface_hub import HfApi

api = HfApi()  # HF_ENDPOINT and HF_TOKEN point at Hangar
c = api.create_collection("Embedding models", namespace="acme", private=True)
c = api.add_collection_item(c.slug, item_id="sentence-transformers/all-MiniLM-L6-v2", item_type="model", note="Default")
c = api.add_collection_item(c.slug, item_id="acme/reviews", item_type="dataset")
api.update_collection_item(c.slug, c.items[1].item_object_id, position=0, note="Evaluation set")
api.update_collection_metadata(c.slug, title="Embedding models we support")
for item in api.get_collection(c.slug).items:
    print(item.item_type, item.item_id, item.note)
api.list_collections(owner="acme")
```

`private=True` makes a `private` Collection. Adding an item that is already in the Collection fails, unless you pass `exists_ok=True`.

## With the native API

The web UI uses the native API's `/api/hangar/v1/collections` routes, which you can call with a Token:

```sh
curl -H "Authorization: Bearer hgr_..." \
  "https://hangar.example.com/api/hangar/v1/collections?owner=acme"
```

The [native API reference](../reference/native-api.md) lists every route, including adding, moving and removing items.

## Limits

| Limit | Value |
|---|---|
| Title | 60 characters. The Collection's address, its slug, comes from the title. |
| Description | 150 characters |
| Note on an item | 500 characters |
| Items in a Collection | 1,000 |
