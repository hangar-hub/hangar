---
title: Uninstall Hangar and remove its data
description: "Remove Hangar installed with Docker, Docker Compose, Helm, Homebrew or the binary, delete its data, and clean up the hangar CLI on a workstation."
---

# Uninstall Hangar

Removing Hangar's containers or binary leaves its data where it is. Each section below says how to remove the data too. [Where Hangar keeps data](../concepts/data.md) lists every location.

## Keep a copy first

To keep your models and datasets, back up before you remove anything:

- `hangar admin backup` writes the database and every LFS object to a folder; see [Backup and restore](backup.md).
- Or copy the Blob store folder or bucket. Large files are stored whole, under their SHA-256, so you can read them without Hangar.

Keep the Instance key with the backup if you might restore it, but not in the same place.

## Docker

```sh
docker rm -f hangar
docker volume rm hangar            # the database, the Blob store and secrets.yaml
docker volume rm hangar-backups    # if you made backups into this volume
docker image rm ghcr.io/hangar-hub/hangar:latest   # or the tag you ran
```

## Docker Compose

From `deploy/compose`, with the file you started:

```sh
docker compose -f quickstart.yaml down       # removes the containers, keeps the volumes
docker compose -f quickstart.yaml down -v    # also deletes the example's volumes, and with them the data
```

`down -v` deletes the named volumes the file declares: `hangar-data` in `quickstart.yaml` and `oidc-keycloak.yaml`, and `postgres-data` and `minio-data` in `postgres-minio.yaml` and `scale.yaml`, which also has its own `hangar-data`. The `.env` file holds the passwords and the Instance key; delete it once you no longer need the data.

## Helm

```sh
helm uninstall hangar --namespace hangar
```

The chart keeps two resources on purpose, so a reinstall over the same data keeps working: the PersistentVolumeClaim (with persistence on) and the Secret with the generated Instance key and admin password. For a release named `hangar`, delete them with:

```sh
kubectl delete pvc hangar --namespace hangar
kubectl delete secret hangar-generated --namespace hangar
```

Whether deleting the PersistentVolumeClaim also deletes the volume depends on its StorageClass's reclaim policy. If you used Postgres, an S3 bucket or your own Secret (`existingSecret`), the chart did not create them, so delete them yourself.

## Binary and Homebrew

Stop the server, then delete the binary, or uninstall the cask:

```sh
sudo rm /usr/local/bin/hangar
brew uninstall --cask hangar
```

Then delete the folders your configuration names: `server.data_dir`, `database.dsn` with SQLite, `storage.fs.path` and `git.cache_dir`. With the defaults those are `/var/lib/hangar` and `/var/cache/hangar/git`; the [binary guide](../get-started/binary.md#run-a-server) puts them under `~/hangar` and `~/.cache/hangar/git`.

## The CLI on a workstation

1. Sign out, which removes the saved Token from the OS keychain and the configuration file:

    ```sh
    hangar logout
    ```

    The Token stays valid until you revoke it: open **Settings**, **Tokens** in the web UI and revoke it, and any other Tokens you no longer use.

2. Remove the models `hangar lmstudio install` put into LM Studio. `--prune` also deletes their files from the Hugging Face cache when no other install uses them:

    ```sh
    hangar lmstudio list
    hangar lmstudio remove <repo> --prune
    ```

3. Under **Settings**, **Ollama keys**, remove the key you registered. Models you pulled into Ollama stay in Ollama until you remove them with `ollama rm`.
4. Delete `~/.config/hangar`.

Files that `hangar pull` downloaded are in the shared Hugging Face cache (`~/.cache/huggingface/hub` by default), where Python libraries use them too. Remove them from there as you would files downloaded from huggingface.co.
