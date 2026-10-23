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

For each Repo in the cache, Hangar creates a Mirror of the huggingface.co Repo with the same ID at the cached Commit: the snapshot that `main` points at, or the newest snapshot if the cache does not record `main`. The Instance fetches the history, which is small, and the CLI uploads only the cached large files that the Mirror is missing. Files you have not cached show as **Not in arsenal**, and you can pull them later.

The import reads every cached snapshot of a Repo, not only that one, and matches files by content rather than by name: the SHA-256 of large files and the git blob ID of the others. A file counts as matched when the Mirror has the same content at the cached Commit, the default branch or the Commit of any other cached snapshot, under any path.

If the Repo is already a Mirror, the import fills in the files it is missing. When the cached Commit is newer than the Mirror's last sync, Hangar fetches its history first. The uploaded files are added to the Mirror at the Commit they matched, so files that have changed on huggingface.co since are not downloaded again.

With `--offline`, or for a Repo huggingface.co does not have, each Repo becomes a local Repo with one Commit of the cached files instead, from the snapshot of `main` or else the newest. Add `--namespace` to put those in a Namespace of your choice. An import never commits to a Mirror, because Mirrors are read-only.

### Files that do not match

A file whose content matches nothing in the Mirror is not uploaded. The results count it as not matched, then name it with the reason:

```text
Not matched:
  acme/model: notes.safetensors (snapshot 1a2b3c4d5e6f): no file in the Mirror at Commits 9f8e7d6c5b4a or 0c1d2e3f4a5b has this content
  acme/model: model.safetensors: its content differs from model.safetensors at Commit 9f8e7d6c5b4a and from every other file checked
```

The snapshot is named when the Repo has more than one in the cache. With `--json`, each result lists these files under `not_matched`, with `path`, `reason` and `snapshot`.

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

Each `<publisher>/<repo>` folder that matches a huggingface.co Repo of the same name becomes a Partial mirror holding the files whose content matches a file of the Mirror's default branch, whatever their names. Files that match nothing are reported as not matched, with the reason. Other folders become local Repos.

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
