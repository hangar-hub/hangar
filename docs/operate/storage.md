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
    presign_ttl: 1h
```

- In `redirect` mode, LFS downloads answer with a presigned URL, so file bytes go straight from the store to the client. Clients must reach the store's endpoint; if they cannot, use `proxy`, which streams through Hangar.
- Uploads go straight to presigned part URLs where the client supports it.
- Ollama downloads always stream through Hangar, because Ollama refuses redirects to other hosts.

| Provider | Settings | Notes |
|---|---|---|
| AWS S3 | `endpoint: s3.<region>.amazonaws.com`, `region`, `bucket`; credentials from the environment, the shared file, the instance profile or IRSA | Add a lifecycle rule that aborts incomplete multipart uploads after a few days. |
| MinIO | `endpoint: minio.example.com:9000` (or `http://...`), `use_path_style: true`, `region: us-east-1` | Tested in CI. MinIO expires stale multipart uploads itself (24 hours by default). |
| Ceph RGW | `endpoint` of the gateway, `use_path_style: true` | |
| Cloudflare R2 | `endpoint: <account-id>.r2.cloudflarestorage.com`, `region: auto` | R2 has no egress charges, which suits `redirect` mode. |
| Backblaze B2 | `endpoint: s3.<region>.backblazeb2.com`, `region: <region>` | Use an application key limited to the bucket. |
| Garage | `endpoint` of a Garage node, `region: garage`, `use_path_style: true` | |

The automated suite runs against MinIO; the other providers are configured as above but are not part of CI yet. `hangar admin doctor` checks a write, read and delete, the clock against the store and, in `redirect` mode, that a presigned URL works from the server.

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
