---
title: Share one Hugging Face model cache across a team
description: "Stop every machine downloading the same models: share a Hugging Face cache on a network drive, or serve one copy to the team from Hangar."
---

# Share one Hugging Face model cache across a team

Every machine that runs `huggingface_hub`, `transformers` or the `hf` CLI keeps its own cache, by default in `~/.cache/huggingface/hub`. On a team, that means each person and each server downloads the same multi-gigabyte models from huggingface.co, and each copy takes up its own disk space.

You can keep one copy for everyone in two ways: put the cache folder itself on shared storage, or run Hangar as the team's copy, so every machine downloads from it over your own network into its own cache. You can also combine them.

## Option 1: a cache folder on shared storage

`huggingface_hub` reads the cache location from `HF_HUB_CACHE`, or from `HF_HOME` with `hub` added. Point every machine at a folder on a network file system, such as NFS or SMB:

```sh
export HF_HUB_CACHE=/mnt/models/hub
```

The first machine to load a model downloads it into the folder, and the others find it there. This needs nothing new to run, but the cache was designed as a local folder. Check these points first:

- Every machine that downloads needs write access to the whole folder. `huggingface_hub` takes file locks in the cache while it downloads, so two machines that ask for the same file at once wait for each other only if the file system supports those locks.
- Snapshots link to files in `blobs/`. On a file system that does not handle symbolic links well, set `HF_HUB_DISABLE_SYMLINKS=1` on every machine, so files are copied into `snapshots/` instead. A file can then be stored once per Revision.
- Anyone who can read the folder can read every model in it, including models from private or gated Repos that someone else downloaded with their own token.
- Only machines that mount the share can use it. CI runners, laptops away from the office and Kubernetes nodes each need the mount.
- Each new model or Revision is still downloaded from huggingface.co, with whatever token the person who asked for it had.

To share a cache that machines only read, fill it from one machine, mount it read-only on the others, and set `HF_HUB_OFFLINE=1` there, so the libraries load from the cache without making any requests.

## Option 2: Hangar as the team's copy

Hangar keeps one copy of each model on the Instance and serves it with the Hugging Face Hub API. Each machine keeps its own local cache as usual, and `HF_ENDPOINT` sends its downloads to Hangar instead of huggingface.co:

```sh
export HF_ENDPOINT=https://hangar.example.com
export HF_TOKEN=hgr_...
```

Code does not change, and a Revision pinned to a Commit SHA works as it does on huggingface.co, because each Mirror keeps the upstream Repo ID and Commit SHAs. Compared with a shared folder:

- Downloads go over your own network, and huggingface.co sees one download per file, from Hangar's Pull.
- Each person signs in with their own Token, and [Organisations and Roles](../concepts/permissions.md) decide who can read which Repos, including private and gated ones.
- Anything that can reach the Instance over HTTPS can use it: laptops, CI runners and Kubernetes Pods, with no file system mount.
- The Instance also holds models and datasets you upload, and serves GGUF models to Ollama and LM Studio.

### Set it up

1. [Run an Instance](../get-started/index.md) where the team can reach it.
2. If you already have a large cache, such as a shared folder from option 1, import it, so Hangar does not download those files again:

    ```sh
    hangar import --from-hf-cache /mnt/models/hub
    ```

    Each cached Repo becomes a Mirror of the huggingface.co Repo at the cached Commit. See [Importing an existing cache](import.md).

3. Pull the other models the team uses. `--server-only` fills the Arsenal without downloading to your machine:

    ```sh
    hangar pull Qwen/Qwen3-0.6B --server-only
    ```

    To keep a model up to date, add `--track`. To let a request for a model that is not in the Arsenal start its Pull, an admin can turn on [Pull-through](../concepts/mirrors.md#pull-through).

4. For gated models, save a huggingface.co token under **Settings**, **Upstream tokens**, or set an Instance token for the Upstream. See [Gated models](gated.md).
5. Each person creates a Token under **Settings**, **Tokens**, then sets `HF_ENDPOINT` and `HF_TOKEN`. After `hangar login`, `eval "$(hangar env)"` sets both. See [Point HF_ENDPOINT at Hangar](../get-started/hf-endpoint.md).

## Combine the two

On a cluster of GPU machines that share a file system, you can use both: Hangar as the source, and a shared cache folder that the machines only read.

Fill the shared folder from Hangar on one machine. `hangar pull` writes into a Hugging Face cache in the same layout the Python libraries read:

```sh
hangar pull Qwen/Qwen3-0.6B --cache-dir /mnt/models/hub
```

Then load from the folder on every machine with no network requests:

```sh
export HF_HUB_CACHE=/mnt/models/hub
export HF_HUB_OFFLINE=1
python train.py
```

Only the machine that fills the folder needs write access and a Token. `hangar pull` creates the cache folders readable by its own user and group only, so run it as a user in a group that the readers share.

## Which to choose

| | Shared cache folder | Hangar |
|---|---|---|
| Something new to run | No | One Instance |
| Who can read what | Anyone who can read the folder | Per User, Organisation and Repo |
| Works without a file system mount | No | Yes, over HTTPS |
| Downloads from huggingface.co | Once per new file, by whoever asks first | Once per file, by the Instance |
| Your own models and datasets | No | Yes, with uploads and git |
| Ollama and LM Studio | No | Yes |

## Related pages

- [Python libraries](python.md) and [the hf CLI](hf-cli.md): use Hangar from Python and the shell.
- [Hugging Face rate limits in CI](ci.md): stop CI jobs downloading models from huggingface.co.
- [Kubernetes model loading](kubernetes.md): keep models on a volume across Pod restarts.
- [Where Hangar keeps data](../concepts/data.md): where the Instance and the CLI keep files.
