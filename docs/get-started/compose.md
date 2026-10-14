# Run Hangar with Docker Compose

The repository has four Compose examples in [`deploy/compose`](https://github.com/jamesagarside/hangar/tree/main/deploy/compose). CI starts each of them and runs the smoke test against it.

| File | What it runs |
|---|---|
| `quickstart.yaml` | One Hangar container with SQLite and the filesystem Blob store. |
| `postgres-minio.yaml` | Hangar with Postgres and MinIO, the setup to grow from. The bucket is created on start. |
| `scale.yaml` | Two `web` replicas behind Traefik, two `worker` replicas, Postgres, MinIO and an OpenTelemetry Collector that prints what it receives. |
| `oidc-keycloak.yaml` | Hangar with Keycloak and an imported realm, for trying OIDC sign-in. |

Get the files:

```sh
git clone https://github.com/jamesagarside/hangar.git
cd hangar/deploy/compose
```

## Quickstart

```sh
docker compose -f quickstart.yaml up -d
docker compose -f quickstart.yaml logs hangar   # the generated admin password
```

## Postgres and MinIO

```sh
docker compose -f postgres-minio.yaml up -d
```

Change `POSTGRES_PASSWORD` and `MINIO_ROOT_PASSWORD` (environment variables or an `.env` file next to the Compose file) before you store anything real. The MinIO console is at <http://localhost:9001>.

Downloads stream through Hangar in this example (`storage.s3.download_mode: proxy`), because your browser cannot reach `http://minio:9000`. If MinIO has a public address, set `HANGAR_STORAGE_S3_ENDPOINT` to it and remove `HANGAR_STORAGE_S3_DOWNLOAD_MODE`, so clients download straight from MinIO.

## Scaled out

Every replica needs the same Instance key and first admin password, so set them first:

```sh
export HANGAR_INSTANCE_KEY="$(openssl rand -base64 32)"
export HANGAR_ADMIN_PASSWORD="$(openssl rand -base64 18)"
docker compose -f scale.yaml up -d
docker compose -f scale.yaml logs -f otel-collector
```

A one-off `migrate` service applies database migrations before the `web` and `worker` replicas start. Hangar is at <http://localhost:8080> and the Traefik dashboard at <http://localhost:8082>. See [Scaling](../operate/scaling.md).

## OIDC with Keycloak

```sh
docker compose -f oidc-keycloak.yaml up -d
```

Open <http://localhost:8080> and choose **Sign in with Keycloak**. The realm has two users: `alice` (password `alice-password`, an Instance admin through the `hangar-admins` group) and `bob` (password `bob-password`, a write Member of the `ml` Organisation through the `ml-team` group). Keycloak answers at `http://keycloak.localhost:8081`; browsers resolve any `*.localhost` name to your machine, and the Hangar container reaches Keycloak at the same address, so the issuer matches on both sides. See [OIDC with Keycloak](../operate/oidc/keycloak.md).

## Use another image

Every example takes `HANGAR_IMAGE`, `HANGAR_PORT` and `HANGAR_PUBLIC_URL`:

```sh
HANGAR_IMAGE=ghcr.io/jamesagarside/hangar:1.0.0 HANGAR_PORT=9090 \
  HANGAR_PUBLIC_URL=http://localhost:9090 docker compose -f quickstart.yaml up -d
```
