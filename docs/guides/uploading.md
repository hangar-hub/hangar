# Upload models and datasets

You can upload in five ways. All of them create real Commits, and a large file that already exists anywhere in the Instance is not stored twice.

| Way | Use it for |
|---|---|
| `huggingface_hub` (`upload_file`, `upload_folder`, `create_commit`) | Code that already uploads to the Hub. |
| The `hf` CLI (`hf upload`) | One-off uploads from a shell. |
| `hangar push` | Folders of any size, with resumable multipart uploads. |
| `git push` with git-lfs | Repos you already manage with git. See [git and git-lfs](git.md). |
| The web UI | Drag and drop files or folders. Uploads go straight to the Blob store in parallel parts and resume after a dropped connection. |

## With huggingface_hub

```python
from huggingface_hub import HfApi

api = HfApi()  # HF_ENDPOINT and HF_TOKEN point at Hangar
api.create_repo("acme/my-model", private=True, exist_ok=True)
api.upload_folder(repo_id="acme/my-model", folder_path="./weights", commit_message="First weights")
api.upload_file(path_or_fileobj="README.md", path_in_repo="README.md", repo_id="acme/my-model", create_pr=True)
```

### With and without hf_xet

`huggingface_hub` installs `hf_xet` by default on x86_64 and arm64, and then uploads every large file through the Xet protocol. Hangar accepts Xet uploads and stores whole files, so this works with no setup. Downloads always use plain HTTP.

If you turned Xet uploads off on the Instance (`hfapi.xet: reject`), set `HF_HUB_DISABLE_XET=1` on clients, so they use classic LFS uploads instead.

Files over `lfs.multipart_threshold` (64 MiB by default) go up in parts, and a single file can be up to 5 TiB.

## With hangar push

```sh
hangar push ./my-finetune acme/my-finetune --create --private -m "First upload"
hangar push ./data datasets/acme/reviews --branch march
```

`hangar push` hashes every file, asks the Instance which ones it needs, uploads only those, in parts, then makes one Commit. If an upload stops part-way, run the same command again: parts already stored are not sent again. A file that still fails after its retries does not stop the others: the command uploads the rest, names the files that failed and makes no Commit, so the next run sends only those files and then commits. `.git` and `huggingface_hub`'s `.cache/huggingface` folder are skipped; add `--include` and `--exclude` patterns for more control, or `--create-pr` to open a pull request instead.

Symbolic links are followed only to files inside the folder, or to files in a Hugging Face cache, such as the files of a cache snapshot or the files `hangar lmstudio` links into LM Studio's folder. `hangar push` skips any other link and names it, so a folder you did not make yourself cannot send a file from elsewhere on your machine. Copy such a file into the folder to upload it.

## Your own models, not on any Upstream

Repos you upload are ordinary Repos: they have the same history, browsing, indexing and security scanning as Mirrors, and you can share them through Organisations, collaborators or visibility. Repos in the Arsenal that came from somewhere else can be [imported](import.md) without uploading them through the network twice.
