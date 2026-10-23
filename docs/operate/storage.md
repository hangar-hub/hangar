---
title: Store models on S3, MinIO or local disk
description: "Choose where Hangar stores model files: the filesystem or any S3-compatible store, with multipart upload clean-up, garbage collection and quotas."
---

# Storage backends

The Blob store holds LFS objects, git packs and upload staging. Choose the filesystem or any S3-compatible store with `storage.backend`. Keys are the same on both, so you can move between them with [backup and restore](backup.md).

## Filesystem

```yaml
storage:
  backend: fs
  fs:
    path: /var/lib/hangar/blobs
```

Writes go to a temporary file that is renamed into place, so readers never see partial objects, and nothing relies on file locks. That makes NFS and SMB mounts usable, also from several processes, because the database coordinates everything. Downloads stream through Hangar.

## S3-compatible stores

```yaml
storage:
  backend: s3
  s3:
    endpoint: s3.eu-west-2.amazonaws.com   # host[:port]; prefix http:// for plain HTTP
    region: eu-west-2
    bucket: hangar
    prefix: ""                             # optional key prefix inside the bucket
    access_key_id: ""                      # empty: the default credential chain, IRSA or Workload Identity
    secret_access_key: ""
    use_path_style: false
    download_mode: redirect                # redirect | proxy
    upload_mode: direct                    # direct | proxy
    presign_ttl: 1h
    retries: 5                             # resend a failed part or object upload; 0 = never
    stall_timeout: 2m                      # cancel and retry a request that makes no progress
    startup_wait: 5m                       # keep trying to reach the bucket at start; 0 = exit at once
```

- In `redirect` mode, LFS downloads and zip exports answer with a presigned URL, so file bytes go straight from the store to the client. Clients must reach the store's endpoint; if they cannot, use `proxy`, which streams through Hangar.
- In `direct` upload mode, large-file uploads from git-lfs, `huggingface_hub`, `hangar push` and the web UI go straight to presigned URLs, and the UI's Content Security Policy allows the store's origin. Clients must reach the store's endpoint; if they cannot, set `upload_mode: proxy`, and uploads stream through Hangar into the store. Presigned upload URLs sign no headers, because `huggingface_hub` sends none, and write to a staging key under `tmp/uploads/`; Hangar checks the SHA-256 and size of what arrives and copies it into place server-side before anyone can download it.
- Ollama downloads always stream through Hangar, because Ollama refuses redirects to other hosts.
- At start, a server or worker that cannot reach the bucket keeps trying for `startup_wait`, logging `waiting for the Blob store` with the endpoint, the bucket and the error on each attempt, and then exits. While it waits, `/healthz` answers 200 and `/readyz` answers 503, so an object store that starts after Hangar, as in some GitOps installs, does not put Hangar into a restart back-off. The filesystem backend is not retried.

| Provider | Settings | Notes |
|---|---|---|
| AWS S3 | `endpoint: s3.<region>.amazonaws.com`, `region`, `bucket`; credentials from the environment, the shared file, the instance profile or IRSA | Add a lifecycle rule that aborts incomplete multipart uploads after a few days. |
| MinIO | `endpoint: minio.example.com:9000` (or `http://...`), `use_path_style: true`, `region:` the region your MinIO is configured with | Tested in CI. Use MinIO's own region: `us-east-1` only when MinIO has none set, which is its default. If it has one, such as `eu-west-2` set with `MINIO_SITE_REGION`, requests signed for another region fail. `mc admin config get <alias> site` shows it. Behind nginx, see [MinIO behind nginx](proxies.md#minio-behind-nginx). MinIO expires stale multipart uploads itself (24 hours by default). |
| Ceph RGW | `endpoint` of the gateway, `use_path_style: true` | |
| Cloudflare R2 | `endpoint: <account-id>.r2.cloudflarestorage.com`, `region: auto` | R2 has no egress charges, which suits `redirect` mode. |
| Backblaze B2 | `endpoint: s3.<region>.backblazeb2.com`, `region: <region>` | Use an application key limited to the bucket. |
| Garage | `endpoint` of a Garage node, `region: garage`, `use_path_style: true` | |

### Slow or unreliable links

Hangar often runs on home hardware and uploads to a cloud bucket over a home connection, where a connection can be reset or stall part of the way through a request. Hangar sends each multipart part and each single upload again when the connection fails, makes no progress for `stall_timeout`, or the store answers with a server error, `SlowDown` or `RequestTimeout`. It waits about 1 second before the first retry and doubles the wait each time, up to 30 seconds, for up to `retries` retries. Errors that describe the request itself, such as `AccessDenied` or a missing upload, are not retried.

So that a retry can send the same bytes again, Hangar copies each part to a file in the temporary directory (`TMPDIR`, `/tmp` in the container image) while it sends it, and deletes the file when the part is done. Each upload in progress uses up to one part of space there: 16 MiB for objects up to 156 GiB, and more for larger objects. Parts read from a file or from memory are rewound instead of copied.

On a congested link, raise `stall_timeout` rather than `retries`: after the last bytes leave Hangar they can still wait in the operating system's send buffer and the router's queue, and the store only answers once they arrive.

The automated suite runs against MinIO, and passed against AWS S3 (`eu-west-2`) on 2026-10-15; the other providers are configured as above but are not part of CI yet. `hangar admin doctor` checks a write, read and delete, the clock against the store and, in `redirect` mode, that a presigned URL works from the server.

## Multipart uploads left behind

An upload that a client abandons is aborted when its row expires (`lfs.upload_url_ttl`). On S3, also add the bucket lifecycle rule `AbortIncompleteMultipartUpload` (AWS) or rely on the store's own expiry (MinIO), for uploads whose row was lost.

## Garbage collection and quotas

A scheduled `gc` Job (`gc.schedule`, 03:00 daily by default) deletes LFS objects no Repo references once they are older than `gc.grace_period` (7 days), and git data that is no longer reachable. Admins can see usage by Namespace and set quotas under **Admin**, **Storage**.

To collect at once, select **Run GC** on that page, or run it on the server host with the server configuration:

```sh
hangar admin gc            # LFS objects and git data; add --git or --lfs for one of them
hangar admin gc --queue    # queue a gc Job for the running Instance's workers (Postgres)
```

`hangar admin gc` reports what it deleted and how much the Blob store holds afterwards. With SQLite, stop the server first.
