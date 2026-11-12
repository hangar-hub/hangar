---
title: Point llama.cpp at a self-hosted Hugging Face Hub
description: "Download GGUF models from Hangar into llama-server and llama-cli with MODEL_ENDPOINT and -hf, or from a private Repo by its URL with a Token."
---

# Use llama.cpp

llama.cpp's `llama-server`, `llama-cli` and `llama-completion` download GGUF models from huggingface.co with `-hf <repo>:<quant>`. Set `MODEL_ENDPOINT` to your Instance and they download from Hangar instead. llama.cpp also reads `HF_ENDPOINT` when `MODEL_ENDPOINT` is not set.

This needs llama.cpp b8498 or later. Earlier builds look a model up with a request that Hangar does not answer in the form they expect, so `-hf` fails; update llama.cpp.

## Public Repos

`-hf` works for public Repos on an Instance that allows anonymous reads (`security.anonymous_read`, off by default):

```sh
export MODEL_ENDPOINT=https://hangar.example.com
llama-server -hf bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

- The tag after `:` picks the GGUF file whose name contains it. Without a tag, llama.cpp picks `Q4_K_M`, or `Q8_0` if the Repo has no `Q4_K_M`.
- The quant must be in the Arsenal. For a [Partial mirror](../concepts/mirrors.md#partial-mirrors) without it, pull the quant first with `hangar pull <repo>:<quant> --server-only`.
- Anyone who can reach the Instance can read every public Repo when anonymous reads are on, so use this only on a network you trust.

## Private Repos

llama.cpp sends a Token with its `-hf` requests only if the Token looks like a huggingface.co token, which starts with `hf_`. Hangar Tokens start with `hgr_`, so for a private Repo, or with anonymous reads off, `-hf` fails with `Repository Not Found`.

Download the file by its URL instead, with `-mu`. llama.cpp sends `HF_TOKEN`, or the Token you pass with `-hft`, with that download:

```sh
export HF_TOKEN=hgr_...
llama-server -mu https://hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q4_K_M.gguf
```

The URL is `<Instance>/<repo>/resolve/<revision>/<file>`. The **Files** tab of a Repo in the web UI lists each file's name.

## S3 storage

On S3 storage, set `storage.s3.download_mode: proxy`, so Hangar streams the files itself. See [Storage backends](../operate/storage.md).

In `redirect` mode, Hangar sends each download to a link that the bucket signed. llama.cpp decodes and re-encodes the link's query string when it follows the redirect, so the bucket no longer receives the link it signed. MinIO, for example, refuses the download with 400.

## Errors you may see

| Problem | Cause | Fix |
|---|---|---|
| `GET failed (401): Repository Not Found` | The Repo is private, or anonymous reads are off, and llama.cpp did not send the Token. | Download the file with `-mu` and `HF_TOKEN`, as in [Private Repos](#private-repos). |
| `-mu` fails with `status code: 401` | No Token, or a Token that cannot read the Repo. | Set `HF_TOKEN` to a Token that can read it. |
| The download fails with `status code: 400` or `403` | S3 storage in `redirect` mode. | Set `storage.s3.download_mode: proxy`. |
| `failed to resolve commit` with a connection error | llama.cpp cannot reach `MODEL_ENDPOINT`. In a container, `localhost` is the container itself. | Use an address the container can reach. |

## Related pages

- [Ollama](ollama.md) and [LM Studio](lm-studio.md): the other ways to run GGUF models from Hangar.
- [Pull your first model](../get-started/first-pull.md): pull a whole Repo or one quant.
