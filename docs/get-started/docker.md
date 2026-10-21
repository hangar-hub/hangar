---
title: Self-hosted Hugging Face Hub in Docker
description: "Run Hangar as one Docker container, with SQLite and the filesystem Blob store in a volume, and configure it with environment variables."
---

# Run Hangar with Docker

One container, with SQLite and the filesystem Blob store in a volume.

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
  ghcr.io/hangar-hub/hangar:latest
```

`HANGAR_SERVER_PUBLIC_URL` is the URL clients use to reach Hangar. It appears in git URLs, LFS links and redirects, so set it to the address your users type, such as `https://hangar.example.com` behind a reverse proxy.

On the first start Hangar generates an Instance key and a password for the first admin, saves them in `/var/lib/hangar/secrets.yaml` (mode 0600) and prints them once:

```sh
docker logs hangar
```

```text
Hangar generated these secrets on first start and saved them in /var/lib/hangar/secrets.yaml.
They are shown once. Store them somewhere safe.
  Admin username: hangar-admin
  Admin password: XLhYPMrxhrVxjJDFmpPcPtVy
  Instance key:   kDsZ+ywVEYHQMu+TaL+iXngDASu4CMDPRC0zH0RYkZs=
```

Keep the Instance key: it encrypts stored secrets such as upstream tokens and webhook secrets. Later starts read both from the file.

## The image

- `ghcr.io/hangar-hub/hangar:<version>` and `:latest`, for `linux/amd64` and `linux/arm64`, signed with cosign (see [Security hardening](../operate/security.md#verify-the-image)).
- It runs as UID 65532 with no shell, and works with a read-only root filesystem when `/tmp` and `/var/cache/hangar` are writable:

  ```sh
  docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
    --read-only --tmpfs /tmp --tmpfs /var/cache/hangar:uid=65532,gid=65532 \
    -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
    ghcr.io/hangar-hub/hangar:latest
  ```

- Data lives in `/var/lib/hangar`, the git pack cache in `/var/cache/hangar`.
- The image has a `HEALTHCHECK` that runs `hangar healthcheck`, which calls `/readyz`.

## Configuration

Every configuration key is also an environment variable: `storage.s3.bucket` is `HANGAR_STORAGE_S3_BUCKET`. To use a file instead, mount it at `/etc/hangar/hangar.yaml`, where the image reads it (the image sets `HANGAR_CONFIG` to that path). Environment variables override the file.

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  -v "$PWD/hangar.yaml:/etc/hangar/hangar.yaml:ro" \
  ghcr.io/hangar-hub/hangar:latest
```

Every key is listed in the [configuration reference](../reference/configuration.md).

Next: [First sign-in](first-login.md).
