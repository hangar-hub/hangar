---
title: Import a Hugging Face cache, Ollama or LM Studio
description: "Import models you already have into Hangar without downloading them again: from a Hugging Face cache, a folder, LM Studio or Ollama."
---

# Import an existing cache

`hangar import` adds models you already have on disk to the Arsenal without downloading them again. Uploads go through the Instance's API, so it works against any Hangar you can write to. A file that fails to upload does not stop the import: the other files of the same Repo still go up and are added to its Mirror, the other Repos are imported, and the results name what failed. Run the same command again to send only what failed.

## A Hugging Face cache

```sh
hangar import --from-hf-cache                     # ~/.cache/huggingface/hub, or $HF_HUB_CACHE
hangar import --from-hf-cache /mnt/models/hub
```

For each Repo in the cache, Hangar creates a Mirror of the huggingface.co Repo with the same ID at the cached Commit: the Instance fetches the history, which is small, and the CLI uploads only the cached large files whose SHA-256 matches. Files you have not cached show as **Not in arsenal**, and you can pull them later.

If the Repo is already a Mirror, the import fills in the files it is missing. When the cached Commit is newer than the Mirror's last sync, Hangar fetches its history first. The uploaded files are added to the Mirror at the cached Commit, so files that have changed on huggingface.co since are not downloaded again.

With `--offline`, or for a Repo huggingface.co does not have, each Repo becomes a local Repo with one Commit of the cached files instead. Add `--namespace` to put those in a Namespace of your choice. An import never commits to a Mirror, because Mirrors are read-only.

## A folder

```sh
hangar import --from-dir ./weights --repo acme/my-model
```

This is the same as `hangar push ./weights acme/my-model --create`.

## LM Studio

```sh
hangar import --from-lmstudio
hangar import --from-lmstudio ~/lm-models
```

Each `<publisher>/<repo>` folder that matches a huggingface.co Repo of the same name becomes a Partial mirror holding the files whose SHA-256 matches; files that differ are reported as not matched. Other folders become local Repos.

## Ollama

```sh
hangar import --from-ollama --namespace acme
```

Each model and tag in Ollama's store (`~/.ollama/models`, or `$OLLAMA_MODELS`) becomes a local Repo, such as `acme/llama3.2` for `llama3.2:latest` and `acme/llama3.2-1b` for `llama3.2:1b`, with the GGUF file and Ollama's `template`, `system`, `params` and `LICENSE` files. Hangar's Ollama endpoint then serves it back with the same template.

## Choose which models

By default, a source imports every model it holds. To import only some, give `--only` once for each model, as the source names it:

```sh
hangar import --from-hf-cache --only Qwen/Qwen2.5-7B-Instruct --only 'bartowski/*'
hangar import --from-lmstudio --only lmstudio-community/gemma-3-12b-it-GGUF
hangar import --from-ollama --only llama3.2:1b --only 'qwen3:*'
```

- For a Hugging Face cache, give the Repo ID. Datasets start with `datasets/`, such as `datasets/acme/data`.
- For LM Studio, give the `<publisher>/<repo>` folder, or the Repo ID it becomes.
- For Ollama, give the model and tag as `ollama list` shows them. A model without a tag means `:latest`, as in Ollama, so use `llama3.2:*` for every tag.

Patterns can use `*` and `?`, which do not match `/`, and ignore case. If a pattern matches nothing, the import stops before it uploads anything and names the pattern, so a typo does not leave a model out. `--only` does not apply to `--from-dir`.

## Symbolic links

Every source follows symbolic links only to files inside the folder it reads, or to files in a Hugging Face cache. Other links are skipped and named on stderr, as `hangar push` does.

## Check first

Every source takes `--dry-run`, which lists what would be imported, and `--json`.
