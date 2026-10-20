---
description: "Pull your first model from huggingface.co into Hangar from the web UI or the CLI, including gated and private Repos."
---

# Pull your first model

A Pull fetches a Repo, or some of its files, from an Upstream such as huggingface.co into the Arsenal. It keeps the upstream ID and Commit SHAs, so code that names `org/model` works against Hangar unchanged.

## From the web UI

1. Open **Discover** and search by name, or enter a full Repo ID such as `hf-internal-testing/tiny-random-bert`.
2. Open a result, or for a Repo ID select **Open hf-internal-testing/tiny-random-bert**. huggingface.co search does not list every Repo (it leaves out `hf-internal-testing`, for example), so a Repo ID opens the Repo directly. Hangar shows the files, their sizes, any GGUF quants and the Dependencies it would pull.
3. Select **Pull into arsenal**. The Pull runs as a Job; follow it under **Jobs**.

For a GGUF Repo, the quant picker lets you pull one quantisation, such as `Q4_K_M`, instead of every file. The Repo then becomes a Partial mirror: the full history is there, and files you did not pull show as **Not in arsenal** with a Pull action.

## From the CLI

```sh
hangar pull hf-internal-testing/tiny-random-bert
hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

`hangar pull` asks the Instance to Pull what is missing, shows the progress, then downloads the files into your local Hugging Face cache, where Python libraries find them. Add `--server-only` to fill the Arsenal without downloading to your machine.

## Gated and private Repos

Pulls of gated or private Repos need your huggingface.co token. Save it under **Settings**, **Upstream tokens**; Pulls you start use it. See [Gated models](../guides/gated.md).

Next: [Point `HF_ENDPOINT` at Hangar](hf-endpoint.md).
