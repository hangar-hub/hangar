---
title: Where Hangar keeps your data
description: "Where Hangar keeps its data on the server and on your machine: the database, the Blob store layout, secrets, caches, and how to get files out without Hangar."
---

# Where Hangar keeps data

## On the server

| What | Default location | Holds |
|---|---|---|
| The database | `database.dsn`: `/var/lib/hangar/hangar.db` with SQLite, or your Postgres database | Everything except file contents: Repos and their refs, users, Organisations, Tokens (as hashes), settings, Jobs, sessions and the audit log. SQLite also keeps a lock file next to it, `hangar.db.lock`. |
| The Blob store | `storage.fs.path`: `/var/lib/hangar/blobs`, or a bucket | LFS objects, git data and upload staging. See the layout below. |
| `secrets.yaml` | `server.data_dir`: `/var/lib/hangar` | The Instance key and the first admin's password, if Hangar generated them on first start. Mode 0600. |
| The git pack cache | `git.cache_dir`: `/var/cache/hangar/git` | Copies of git packs for Repos in use. Disposable: delete it whenever Hangar is stopped. |
| Temporary files | `TMPDIR`: `/tmp` in the image | Upload parts while they are sent to S3, deleted when each part is done. |

In the container image, `/var/lib/hangar` is the volume, and `/var/cache/hangar` and `/tmp` can be a tmpfs.

The Instance key encrypts the secrets Hangar stores in the database, such as users' Upstream tokens, the Instance's token for each Upstream and webhook secrets, and signs the URLs Hangar hands out. Without it, a restored database cannot decrypt them. Keep it with your backups, but not in the same place; see [Backup and restore](../operate/backup.md).

## The Blob store layout

The keys are the same on the filesystem and on S3, under `storage.s3.prefix` if you set one:

| Key | Holds |
|---|---|
| `lfs/sha256/<first 2>/<next 2>/<sha256>` | Each LFS object, whole and unchanged, named by its SHA-256. One copy per Instance, however many Repos hold it. |
| `git/<Repo UUID>/pack/` | Each Repo's git packs. The Repo's branches and tags are in the database. |
| `xet/xorbs/` | Data that `hf_xet` uploads, kept for `xet.xorb_retention` (28 days) after its last use so later uploads can skip it. |
| `exports/` | Zip exports of Repo folders. |
| `uploads/`, `tmp/` | Uploads in progress: the parts of multipart uploads on the filesystem backend, and uploads staged under `tmp/uploads/` before Hangar checks them. |

`hangar admin backup` copies every key except `uploads/`, `tmp/` and `exports/`.

## On your machine

The `hangar` CLI and the tools it drives keep these:

| What | Where | Holds |
|---|---|---|
| CLI settings | `~/.config/hangar/config.yaml`, or `$XDG_CONFIG_HOME/hangar/` | The Instance URL and your username. The Token too, with mode 0600, when no OS keychain is available. |
| The Token | The OS keychain, under the service name `hangar` and the Instance URL | The Token `hangar login` saved. |
| LM Studio installs | `~/.config/hangar/lmstudio.json`, and links in LM Studio's models folder | What `hangar lmstudio install` put into LM Studio, so `list` and `remove` can find it. |
| Downloaded files | The Hugging Face cache: `$HF_HUB_CACHE`, `~/.cache/huggingface/hub` by default | Files `hangar pull` downloads, in the layout Python libraries read. The cache is shared with every Hugging Face tool, not Hangar's own. |
| Your Ollama key | `~/.ollama/id_ed25519.pub` | Ollama's own key. `hangar ollama key register` sends the public half to the Instance, which lists it under **Settings**, **Ollama keys**. |

## Getting your files out

While Hangar runs, `git clone` gets a Repo with its full history and LFS files, and `hf download` or `hangar pull` gets its files; see [git and git-lfs](../guides/git.md).

Without Hangar running, every large file is still whole in the Blob store at `lfs/sha256/...`, under the SHA-256 that its LFS pointer names, so standard tools can copy it out. A `hangar admin backup` folder holds the same objects under `blobs/`, and every database table as JSON lines under `db/`. To move to another Instance or backend, [back up and restore](../operate/backup.md); to remove Hangar, see [Uninstall Hangar](../operate/uninstall.md).
