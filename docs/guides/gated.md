# Gated models

## Pulling gated models from huggingface.co

Gated and private Repos on huggingface.co need a token from an account that has access.

1. On huggingface.co, accept the model's conditions and create a read token.
2. In Hangar, open **Settings**, **Upstream tokens**, and save it for `huggingface`.

Pulls you start then use your token. Hangar encrypts saved tokens with the Instance key and never shows them again. An admin can also set an Instance token for an Upstream (`upstreams.<id>.token`), which Pulls use when the user has none.

## Gating your own Repos

An admin of a Repo can require people to ask for access under the Repo's **Settings**:

- **Automatic** accepts every request straight away, after the person fills in the form.
- **Manual** queues requests for an admin to accept or reject under **Settings**, **Access requests**.

The **Settings** tab also takes a message to show people asking for access. To add fields to the request form, such as a company name, a country or a checkbox to accept terms, set `gating.fields` with the [native API](../reference/native-api.md). Field types are `text`, `checkbox`, `country`, `date` and `select`:

```sh
curl -X PATCH https://hangar.example.com/api/hangar/v1/repos/models/acme/my-model \
  -H "Authorization: Bearer hgr_..." -H "Content-Type: application/json" \
  -d '{"gating": {"fields": [{"name": "Company", "type": "text", "required": true},
                            {"name": "I accept the terms", "type": "checkbox", "required": true}]}}'
```

Saving gating in the **Settings** tab keeps the fields. Gating works through the Hub API too: `huggingface_hub` raises `GatedRepoError` until access is granted, as it does on huggingface.co.
