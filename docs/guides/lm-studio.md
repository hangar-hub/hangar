---
title: Install models into LM Studio from your own hub
description: "Install models from Hangar into LM Studio with hangar lmstudio install, which downloads them and links them into LM Studio's models folder."
---

# Use LM Studio

LM Studio reads models from a folder laid out as `<publisher>/<repo>/<files>`. `hangar lmstudio install` downloads a model into your local Hugging Face cache and links it into that folder:

```sh
hangar lmstudio install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
hangar lmstudio install mlx-community/SmolLM-135M-Instruct-4bit
```

- For a GGUF Repo, it installs one quant group and any `mmproj-*.gguf` projector; without a quant it picks `Q4_K_M`, or the closest one available.
- MLX and other Repos are installed whole.
- Files are symbolic links into the cache on macOS and Linux, so the disk is used once even when Python libraries use the same model. Use `--link-mode hardlink` or `--link-mode copy` if LM Studio's folder is on another disk, or on Windows (where the default is `hardlink`).

Restart LM Studio, or open **My Models**, to see them.

## Where the models folder is

In order:

1. `--lmstudio-dir`, if you give it.
2. The `downloadsFolder` setting in `settings.json` in LM Studio's home folder. The home folder is the path in `~/.lmstudio-home-pointer` if that file exists, otherwise `~/.cache/lm-studio` if it exists, otherwise `~/.lmstudio`. This is how LM Studio itself finds its home.
3. `models` in that home folder.

## Keep track

```sh
hangar lmstudio list            # what is installed, and whether a newer Revision exists
hangar lmstudio remove bartowski/Llama-3.2-3B-Instruct-GGUF
hangar lmstudio remove bartowski/Llama-3.2-3B-Instruct-GGUF --prune   # also free the cache
```

The CLI records installs in `~/.config/hangar/lmstudio.json`. `--prune` deletes the model's cached files unless another install uses them; Python code that used the same cached files downloads them again next time.

To add models you already have in LM Studio to the Arsenal, see [Importing an existing cache](import.md).
