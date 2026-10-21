---
description: "Run Hangar with Docker Compose: a quickstart, Postgres with MinIO, a scaled-out Instance, and OIDC sign-in with Keycloak."
---

# Run Hangar with Docker Compose

The repository has four Compose examples in [`deploy/compose`](https://github.com/hangar-hub/hangar/tree/main/deploy/compose). CI starts each of them and runs the smoke test against it.

| File | What it runs |
|---|---|
| `quickstart.yaml` | One Hangar container with SQLite and the filesystem Blob store. |
| `postgres-minio.yaml` | Hangar with Postgres and MinIO, the setup to grow from. The bucket is created on start. |
| `scale.yaml` | Two `web` replicas behind Traefik, two `worker` replicas, Postgres, MinIO and an OpenTelemetry Collector that prints what it receives. |
| `oidc-keycloak.yaml` | Hangar with Keycloak and an imported realm, for trying OIDC sign-in. |

Get the files:

```sh
git clone https://github.com/hangar-hub/hangar.git
cd hangar/deploy/compose
```

## Create the secrets

The examples have no default passwords: every secret comes from a variable, and Compose stops with an error that names the variable if it is missing. Create them once, in a `.env` file next to the Compose files, which Docker Compose reads for every example in the folder:

```sh
(umask 077 && set -C && cat > .env) <<EOF
HANGAR_INSTANCE_KEY=$(openssl rand -base64 32)
HANGAR_ADMIN_PASSWORD=$(openssl rand -base64 18)
POSTGRES_PASSWORD=$(openssl rand -hex 24)
MINIO_ROOT_PASSWORD=$(openssl rand -hex 24)
KEYCLOAK_ADMIN_PASSWORD=$(openssl rand -hex 24)
HANGAR_OIDC_CLIENT_SECRET=$(openssl rand -hex 24)
EOF
```

`umask 077` makes the file readable by you only, and `set -C` stops the command from overwriting a `.env` you already have: Postgres and MinIO keep the passwords they started with, so changing them later locks Hangar out of its own data. The `.env` file is in `.gitignore`. Keep a copy of `HANGAR_INSTANCE_KEY`, which encrypts stored secrets.

| Variable | Used by |
|---|---|
| `HANGAR_ADMIN_PASSWORD` | Every example: the password of the first admin, `hangar-admin`. `quickstart.yaml`, `postgres-minio.yaml` and `oidc-keycloak.yaml` generate one and print it once if it is not set; `scale.yaml` needs it. |
| `HANGAR_INSTANCE_KEY` | `scale.yaml`, where every replica needs the same key. The other examples generate one into their data volume. |
| `POSTGRES_PASSWORD`, `MINIO_ROOT_PASSWORD` | `postgres-minio.yaml` and `scale.yaml`. |
| `KEYCLOAK_ADMIN_PASSWORD`, `HANGAR_OIDC_CLIENT_SECRET` | `oidc-keycloak.yaml`. |

Only Hangar's port is published on every network interface. The MinIO console, the Traefik dashboard and Keycloak are published on `127.0.0.1`, so you can open them on this machine and nobody else on your network can. Postgres and the MinIO API are not published at all.

## Quickstart

```sh
docker compose -f quickstart.yaml up -d
docker compose -f quickstart.yaml logs hangar   # the generated admin password, if .env does not set one
```

## Postgres and MinIO

```sh
docker compose -f postgres-minio.yaml up -d
```

Sign in as `hangar-admin` with `HANGAR_ADMIN_PASSWORD`. The MinIO console is at <http://127.0.0.1:9001> (user `hangar`, password `MINIO_ROOT_PASSWORD`). MinIO's root user can read and change every LFS object and all git data without going through Hangar's permissions, so do not publish the console on other interfaces.

Downloads and uploads go through Hangar in this example (`storage.s3.download_mode: proxy` and `storage.s3.upload_mode: proxy`), because your browser and your clients cannot reach `http://minio:9000`. If MinIO has a public address, set `HANGAR_STORAGE_S3_ENDPOINT` to it and remove `HANGAR_STORAGE_S3_DOWNLOAD_MODE` and `HANGAR_STORAGE_S3_UPLOAD_MODE`, so clients download from and upload to MinIO directly.

## Scaled out

```sh
docker compose -f scale.yaml up -d
docker compose -f scale.yaml logs -f otel-collector
```

Every replica reads the same `HANGAR_INSTANCE_KEY` and `HANGAR_ADMIN_PASSWORD` from `.env`. A one-off `migrate` service applies database migrations before the `web` and `worker` replicas start. Hangar is at <http://localhost:8080> and the Traefik dashboard at <http://127.0.0.1:8082>. See [Scaling](../operate/scaling.md).

## OIDC with Keycloak

```sh
docker compose -f oidc-keycloak.yaml up -d
```

Open <http://localhost:8080> and choose **Sign in with Keycloak**. The realm has two users: `alice` (password `alice-password`, an Instance admin through the `hangar-admins` group) and `bob` (password `bob-password`, a write Member of the `ml` Organisation through the `ml-team` group). Keycloak answers at `http://keycloak.localhost:8081`; browsers resolve any `*.localhost` name to your machine, and the Hangar container reaches Keycloak at the same address through a network alias, so the issuer matches on both sides. Keycloak is published on `127.0.0.1` only, so these example users and its admin console (user `admin`, password `KEYCLOAK_ADMIN_PASSWORD`) are reachable from this machine only. The realm reads the `hangar` client's secret from `HANGAR_OIDC_CLIENT_SECRET` and its redirect URI from `HANGAR_PUBLIC_URL`, so sign-in works on another port too. See [OIDC with Keycloak](../operate/oidc/keycloak.md).

## Use another image

Every example takes `HANGAR_IMAGE`, `HANGAR_PORT` and `HANGAR_PUBLIC_URL`. `HANGAR_PUBLIC_URL` defaults to `http://localhost:` followed by `HANGAR_PORT`; set it when people reach Hangar at another address.

```sh
HANGAR_IMAGE=ghcr.io/hangar-hub/hangar:1.2.3 HANGAR_PORT=9090 \
  docker compose -f quickstart.yaml up -d
```

The examples run `ghcr.io/hangar-hub/hangar:latest` by default. On an Instance other people rely on, pin a version instead: `1.2.3` for one release, or `1.2` to get patch releases of 1.2 only. Put `HANGAR_IMAGE` in `.env` to keep the pin, and change it when you [upgrade](../operate/upgrades.md). The [releases page](https://github.com/hangar-hub/hangar/releases) lists every version.
