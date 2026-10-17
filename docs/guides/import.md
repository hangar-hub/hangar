# Import an existing cache

`hangar import` adds models you already have on disk to the Arsenal without downloading them again. Uploads go through the Instance's API, so it works against any Hangar you can write to. A file that fails to upload does not stop the import: the other files of the same Repo still go up and are added to its Mirror, the other Repos are imported, and the results name what failed. Run the same command again to send only what failed.

## A Hugging Face cache

```sh
hangar import --from-hf-cache                     # ~/.cache/huggingface/hub, or $HF_HUB_CACHE
hangar import --from-hf-cache /mnt/models/hub
```

For each Repo in the cache, Hangar creates a Mirror of the huggingface.co Repo with the same ID at the cached Commit: the Instance fetches the history, which is small, and the CLI uploads only the cached large files whose SHA-256 matches. Files you have not cached show as **Not in arsenal**, and you can pull them later.

With `--offline`, or for a Repo huggingface.co does not have, each Repo becomes a local Repo with one Commit of the cached files instead. Add `--namespace` to put those in a Namespace of your choice.

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

## Check first

Every source takes `--dry-run`, which lists what would be imported, and `--json`.
