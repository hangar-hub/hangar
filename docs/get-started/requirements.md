---
title: System requirements and sizing
description: "What Hangar needs to run: CPU, memory, disk for models and the git pack cache, temporary space, network access, and what to plan for when you scale out."
---

# System requirements and sizing

Hangar is one binary that runs as a container or on a host. Most of what it needs is disk space for the models and datasets you keep.

## Software

| Install | Needs |
|---|---|
| Container image | Docker, Podman or Kubernetes, on `linux/amd64` or `linux/arm64`. The image includes git. |
| Binary | git 2.39 or later on the `PATH` for the server. The client commands do not need git. |
| Helm chart | Kubernetes 1.29 or later. |
| Scaled out | Postgres (CI tests 18) and an S3-compatible bucket or a shared filesystem. |

[Tested versions and platforms](../reference/compatibility.md) lists what CI runs on.

## CPU and memory

The Helm chart's defaults are a reasonable start for one `web` or `worker` process: a request of 250m CPU and 256 MiB of memory, and a memory limit of 1 GiB. In the [benchmarks](../benchmarks.md), a `web` replica limited to 2 CPUs and 1 GiB peaked at 66 to 115 MiB of memory while it served four concurrent 1 GiB uploads or 140 GiB of downloads. The benchmarks did not measure `worker` processes, which run Pulls, indexing and Parquet exports; watch their memory with the [metrics](../reference/metrics.md) Hangar exports, and raise the limit if your datasets need it.

## Disk

| What | Default location | How much |
|---|---|---|
| The Blob store | `storage.fs.path` (`/var/lib/hangar/blobs`), or a bucket | About the total size of the files you pull and upload. Each LFS object is stored once per Instance, however many Repos hold it, and a Partial mirror holds only the files you pulled. |
| The database | `database.dsn` (`/var/lib/hangar/hangar.db`), or Postgres | Metadata only: Repos, refs, Jobs, users and the audit log, not file contents. |
| The git pack cache | `git.cache_dir` (`/var/cache/hangar/git`) | `git.cache_size`, 10 GiB by default, plus about a quarter more, because the cache goes over its limit while Repos are in use. It is disposable. |
| Temporary files | `TMPDIR` (`/tmp` in the image) | On S3, one part per upload in progress: 16 MiB for objects up to 156 GiB, more for larger ones. The Helm chart gives `/tmp` 2 GiB. |

Space is not freed at once when content goes away. A deleted Repo can be restored for `repos.restore_window` (7 days), and garbage collection deletes LFS objects that no Repo references once they are older than `gc.grace_period` (7 days). See [Storage backends](../operate/storage.md#garbage-collection-and-quotas).

### The pack cache in memory

The [read-only `docker run` example](docker.md#the-image) and `deploy/compose/quickstart.yaml` mount `/var/cache/hangar` as a tmpfs. A tmpfs keeps its files in memory, so the pack cache then uses RAM, not disk. On a small host, give the tmpfs a size and keep the cache below it, or mount a volume there instead:

```sh
docker run -d --name hangar -p 8080:8080 -v hangar:/var/lib/hangar \
  --read-only --tmpfs /tmp --tmpfs /var/cache/hangar:uid=65532,gid=65532,size=4g \
  -e HANGAR_SERVER_PUBLIC_URL=http://localhost:8080 \
  -e HANGAR_GIT_CACHE_SIZE=3GiB \
  ghcr.io/hangar-hub/hangar:latest
```

The Helm chart uses an `emptyDir` for the cache, sized with `cacheSizeLimit` (16 GiB by default), and refuses a limit less than a quarter larger than `git.cache_size`.

## Network

| Direction | What |
|---|---|
| Into Hangar | Clients reach `server.public_url`, port 8080 in the image. The `datasets` library also needs the `datasets-server.<host>` name; see [Datasets](../guides/datasets.md#the-datasets-server-name). |
| Into the store | On S3 in `redirect` download mode or `direct` upload mode (the defaults), clients also reach the bucket's endpoint. Use `proxy` for both if they cannot. |
| Out of Hangar | Upstreams, such as huggingface.co, and the hosts their downloads redirect to; OIDC providers; webhook targets; and an OpenTelemetry endpoint if you set one. Nothing else. |

An [air-gapped Instance](../guides/air-gapped.md) needs no internet access at all.

## Scaled out

On the reference setup in [Scaling](../operate/scaling.md#targets), two `web` replicas with 2 vCPU and 1 GiB each, one `worker`, Postgres and MinIO meet Hangar's targets. Each process opens up to `database.max_open_conns` (20) connections to Postgres, so allow for that many per replica in Postgres's `max_connections`.
