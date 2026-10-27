---
description: "Upgrade Hangar with Docker, Docker Compose, Helm, Homebrew or the binary, with database migrations, checks afterwards, and what to do to go back."
---

# Upgrade Hangar

Hangar follows [semantic versioning](../project/versioning.md). Each release's container image, Helm chart, archives and Homebrew cask carry the same version.

## Before you upgrade

1. Find the version you run: `hangar version`, or **Admin**, **System** in the web UI.
2. Read the [changelog](../project/changelog.md) from your version to the new one. A major version lists what is not backward compatible under **BREAKING CHANGES**. [Notes per version](#notes-per-version) lists anything you need to do.
3. Back up the database, the Blob store and the Instance key; see [Backup and restore](backup.md). A backup is the only way back to the old version.

`hangar admin migrate` applies every pending migration, so you can upgrade from any earlier release of the same major version straight to the latest one.

## Pin a version

`latest` moves to every new release, so a container restart can upgrade an Instance without anyone choosing to. On an Instance other people rely on, run a version tag instead, and change it when you upgrade:

- `ghcr.io/hangar-hub/hangar:1.2.3` stays on one release.
- `ghcr.io/hangar-hub/hangar:1.2` follows patch releases of 1.2 only.

## One container

```sh
docker pull ghcr.io/hangar-hub/hangar:1.2.3
docker stop hangar && docker rm hangar
docker run -d --name hangar ... ghcr.io/hangar-hub/hangar:1.2.3   # the same options as before
```

The data is in the volume, so the new container starts with it. A process in role `all` applies migrations when it starts.

## Docker Compose

Set the new version in `.env` next to the Compose files, for example `HANGAR_IMAGE=ghcr.io/hangar-hub/hangar:1.2.3`, then pull it and recreate the containers:

```sh
docker compose -f postgres-minio.yaml pull
docker compose -f postgres-minio.yaml up -d
```

Use the file you started with. In `scale.yaml`, the one-off `migrate` service runs `hangar admin migrate` with the new image, and Compose starts the new `web` and `worker` replicas once it has finished.

## Binary

Download the new archive and replace the binary, as [Run the hangar binary](../get-started/binary.md) shows, then restart the server. With Homebrew:

```sh
brew upgrade --cask hangar-hub/tap/hangar
```

Binaries built with `go install` report their version as `dev`, so use a release archive for a server you need to keep track of.

## Several processes

1. Run the new version's `hangar admin migrate` once. Migrations only add to the schema within a major version, so processes of the previous version keep working while it runs.
2. Roll the `worker` processes, then the `web` processes. New processes report ready once the schema matches.

## Helm

The chart version is the Hangar version, so upgrading the chart upgrades Hangar:

```sh
helm upgrade hangar oci://ghcr.io/hangar-hub/charts/hangar --version 1.2.3 --namespace hangar -f values.yaml
```

The chart runs the migration in a pre-upgrade Job, or in an init container in each Pod with `migrations.mode: initContainer`, and rolls the Deployments for you. With Argo CD, change `targetRevision` in the Application; see [Run with Argo CD](../get-started/helm.md#run-with-argo-cd).

## Check the upgrade

- `hangar version`, or **Admin**, **System**, shows the new version.
- `/readyz` answers 200. If it answers 503, the body names the check that failed, such as a pending migration.
- `hangar admin doctor` checks the configuration, the database, the Blob store and git end to end; see [Troubleshooting](troubleshooting.md#run-the-doctor).

## Notes per version

| Upgrade | What to do |
|---|---|
| 1.4.0, from any earlier 1.x | Run `hangar admin migrate` as usual (migration `0011`). A Repo's size is now its total: `size_bytes` in the native API counts every LFS object the Repo's Commits reference, including the files a Partial mirror has not pulled, and the new `present_bytes` counts what the Instance holds. Before, `size_bytes` counted only what the Instance held. The migration fills both for existing Repos, and the size sort and the `size_max` filter compare the total. The Hub API's `usedStorage` is still the bytes held. Update `hangar` on client machines for tab completion; see [Turn on tab completion](../guides/hangar-cli.md#turn-on-tab-completion). |
| 1.3.1, from any earlier 1.x | Run `hangar admin migrate` as usual (migrations `0008` to `0010`). On an HTTPS Instance, everyone signs in to the web UI once more, because the session cookie is now `__Host-hangar_session`. UI sessions unused for 7 days now end (`security.session_idle_timeout`). Device logins give a `read` Token unless the user chooses `write`, and their Tokens expire after 30 days. Registering an Ollama key needs a signature from the key: update `hangar` on machines that run `hangar ollama key register`. |
| 1.3.0, from any earlier 1.x | Run `hangar admin migrate` as usual (migrations `0004` to `0007`). |
| 1.1.x or 1.2.x, from any earlier 1.x | Nothing. No release from 1.0.0 to 1.2.3 changed the database schema or the Helm chart's values. 1.2.0 added `ui.docs_url`, which defaults to https://hangar.page/; on an [air-gapped network](../guides/air-gapped.md#keep-the-web-ui-on-the-inside), point it at your copy of the docs. |

## Downgrades

Migrations do not run backwards. To go back to an earlier version, restore a backup made with it.
