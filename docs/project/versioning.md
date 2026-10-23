---
title: Versioning, releases and support
description: "How Hangar numbers its releases, which versions get fixes, and how image tags, the Helm chart, release archives and docs versions line up."
---

# Versioning and support

## Version numbers

Hangar follows [semantic versioning](https://semver.org): versions are `MAJOR.MINOR.PATCH`, such as 1.2.3. release-please proposes each version from the [Conventional Commits](https://www.conventionalcommits.org) merged since the last release, and writes the [changelog](changelog.md):

| Release | Example | What it can contain |
|---|---|---|
| Patch | 1.2.2 to 1.2.3 | Bug fixes, dependency updates and documentation changes. 1.2.1 and 1.2.3 changed only the documentation. |
| Minor | 1.1.0 to 1.2.0 | New features, as well as fixes. |
| Major | 1.x to 2.0.0 | Changes that are not backward compatible. The changelog lists them under **BREAKING CHANGES**. |

There has been no major release since 1.0.0.

Within a major version, database migrations only add to the schema, so processes of the previous version keep working while you [upgrade](../operate/upgrades.md). Migrations do not run backwards: to go back to an earlier version, restore a backup made with it.

The native API has its own version, in its path (`/api/hangar/v1`) and in its [OpenAPI description](../reference/native-api.md) (`1.0.0`). It is the version of the API, not of Hangar.

## Release schedule

There is no fixed schedule. A release goes out when the release pull request that release-please keeps open is merged; the release workflow then publishes the binaries, the container image, the Helm chart, the Homebrew cask and the documentation. The [releases page](https://github.com/hangar-hub/hangar/releases) and the [changelog](changelog.md) list every release.

## Supported versions

Security fixes go into the latest minor release, as the [security policy](security.md) says. Older minor releases do not get fixes, so to get a fix, upgrade to the latest release.

Hangar is maintained by one person. There is no paid support and no guaranteed response time; see [Get help](support.md).

## How versions line up

Every artefact of a release carries the same version:

| Artefact | Names | For release 1.2.3 |
|---|---|---|
| Git tag and GitHub release | `vX.Y.Z` | `v1.2.3` |
| Container image | `ghcr.io/hangar-hub/hangar:X.Y.Z`, `:X.Y` and `:latest`, for `linux/amd64` and `linux/arm64`. There is no tag for a major version alone. | `:1.2.3`, `:1.2` |
| Helm chart | `oci://ghcr.io/hangar-hub/charts/hangar`, whose chart version and `appVersion` are both `X.Y.Z` | `--version 1.2.3` |
| Release archives | `hangar_X.Y.Z_<os>_<arch>.tar.gz`, or `.zip` for Windows | `hangar_1.2.3_linux_amd64.tar.gz` |
| Homebrew cask | `hangar-hub/tap/hangar` | 1.2.3 |
| Documentation | One version per minor release, at `/X.Y/`. `/latest/` is the newest release and `/dev/` follows the `main` branch. | `/1.2/`, also `/latest/` |

To follow patch releases of one minor version, use the `:X.Y` image tag. To change versions only when you choose to, pin `:X.Y.Z`. `:latest` moves to every new release, including minor releases.

## Find the version you run

- `hangar version` prints it, from a release archive, Homebrew or the image: `docker exec hangar hangar version`. Builds made with `go install` report `dev`.
- In the web UI, Instance admins see it under **Admin**, **System**.
- On Kubernetes, `helm list --namespace <namespace>` shows the chart and app version.

## Clients and platforms

Which versions of `huggingface_hub`, git, Ollama, Postgres and Kubernetes each release is tested with, and on which platforms, is in [Tested versions and platforms](../reference/compatibility.md).
