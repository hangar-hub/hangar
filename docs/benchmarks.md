# Benchmarks

These are the load test results for Hangar's scale targets: the targets in the specification's architecture chapter ("Scale targets") and the scenarios in `loadtest/`, run as `loadtest/README.md` describes. They were measured on 17 October 2026, before the v1.0 release.

The numbers come from one shared laptop, not dedicated hardware, so read them as evidence that a target holds or fails on modest hardware rather than as capacity figures. [Deviations](#deviations) lists every way the setup differs from the reference setup.

## Summary

| Target | Result | Pass or fail |
|---|---|---|
| `HEAD /resolve` p99 under 50 ms at 200 requests per second per replica | p99 7 to 28 ms with the host lightly loaded; 87 to 206 ms when other workloads saturated the host | Pass, with the host lightly loaded |
| Model info p99 under 100 ms | p99 12.5 to 62.7 ms | Pass |
| Full listing of a 100,000-file Repo under 10 seconds at p95 | p95 6.15 to 6.79 s | Pass |
| Clone of a 10,000-Commit Repo under 30 seconds at p95 | p95 0.60 to 1.13 s in the four runs with a kept p95 (five runs) | Pass, see [Clone errors](#clone-errors) |
| Web replica memory flat and under 256 MiB during uploads | peak 66 and 81 MiB during four concurrent 1 GiB uploads | Pass (4 GiB uploaded, not 50 GiB) |
| Web replica memory flat and under 256 MiB during downloads | peak 104 and 115 MiB while downloading 140 GiB | Pass |
| Download throughput in `proxy` mode limited by network and storage, not CPU | 1.2 GB/s in total with each replica using 0.35 of its 2 CPUs | Pass |
| Web replica start to ready under 5 seconds | 0.85 to 1.61 s | Pass |
| Dataset Viewer `/rows` p99 under 200 ms on a 10 million row Split | p99 16.1 and 27.8 ms at 50 requests per second | Pass |
| 100,000 Repos per Instance | Not measured: no scenario creates them | Not tested |

## Setup

| Item | Version or value |
|---|---|
| Hangar | `ws/p3-perf` (Phase 3 performance branch), built with `make image IMAGE=hangar:p3perf`; baseline runs used `382cc88` |
| Host | Apple silicon laptop, 12 CPUs, 36 GiB of memory, macOS |
| Containers | Docker Desktop 29.8.2 (a Linux VM with 6 vCPUs and 11.7 GiB), Docker Compose 5.6.0 |
| Load generator | `grafana/k6:1.4.2` in a container limited to 2 CPUs (`--cpus 2`) |
| Stack | `deploy/compose/scale.yaml`: Traefik v3.6, `postgres:18-alpine`, MinIO `RELEASE.2026-08-04T00-00-00Z`, the OpenTelemetry Collector 0.161.0 |
| Web replicas | 2, each limited to 2 CPUs and 1 GiB |
| Worker | 1 |
| Storage | S3 (MinIO), `download_mode` and `upload_mode` both `proxy` |
| Seed data | `loadtest/seed.sh` with the defaults: `loadtest/many-files` (100,000 files) and `loadtest/many-commits` (10,000 Commits, checked with `git rev-list --count`), plus `loadtest/resolve`; a 10 million row dataset `loadtest/rows10m` |

The limits came from a Compose override file next to `scale.yaml`:

```yaml
services:
  traefik:
    command: !override
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --entrypoints.web.address=:18480
      - --entrypoints.web.transport.respondingTimeouts.readTimeout=0
      - --entrypoints.web.transport.respondingTimeouts.writeTimeout=0
    ports: !override
      - "18480:18480"
  web:
    deploy:
      replicas: 2
      resources:
        limits: {cpus: "2", memory: 1g}
  worker:
    deploy:
      replicas: 1
```

## Commands

Start the stack with the override and the image built from the branch:

```sh
HANGAR_IMAGE=hangar:p3perf HANGAR_PUBLIC_URL=http://localhost:18480 \
  docker compose -p hangar-p3perf --env-file .env -f deploy/compose/scale.yaml -f override.yaml up -d
```

Seed as `loadtest/README.md` describes, with a write Token for a `loadtest` user:

```sh
HANGAR_URL=http://localhost:18480 HANGAR_TOKEN=hgr_... HANGAR_USERNAME=loadtest loadtest/seed.sh
```

Run each scenario with k6 in Traefik's network namespace, so `localhost:18480` is the same public URL inside and outside the container:

```sh
docker run --rm -i --cpus 2 --network container:hangar-p3perf-traefik-1 \
  -v "$PWD/loadtest:/lt:ro" -w /lt grafana/k6:1.4.2 run \
  -e HANGAR_URL=http://localhost:18480 -e HANGAR_TOKEN=hgr_... \
  -e HANGAR_NAMESPACE=loadtest -e HANGAR_USERNAME=loadtest -e REPLICAS=2 resolve.js
```

The same command ran `model_info.js`, `tree.js`, `git_clone.js`, `upload.js` (`VUS=4`, 1 GiB each), `download.js` (`VUS=4`, `DURATION=2m`, after `upload.js`) and `viewer_rows.js` (50 requests per second). Memory came from each replica's cgroup (`memory.current`, `memory.peak` and `memory.stat`), sampled every few seconds, and `docker stats`. CPU per request came from each container's cgroup `cpu.stat` before and after a run, divided by the requests k6 sent.

## Results

### HEAD /resolve

400 requests per second in total (200 per replica) for 2 minutes, four `HEAD`s for every `GET`.

| Run | Host load (1 minute) | HEAD p99 | All requests p95 | Median |
|---|---|---|---|---|
| Final, host busy, 1 | 43 | 205.65 ms | 57.08 ms | 4.67 ms |
| Final, host busy, 2 | 54 | 86.99 ms | 25.61 ms | 4.26 ms |
| Final, host busy, 3 | 37 | 103.98 ms | 26.38 ms | 4.39 ms |
| Final, host quiet, 1 | 9 | 12.09 ms | 12.54 ms | 1.64 ms |
| Final, host quiet, 2 | 13 | 12.46 ms | 12.59 ms | 1.74 ms |
| Final, host quiet, 3 | 14 | 6.94 ms | 12.23 ms | 1.40 ms |
| Final, host quiet, 4 | 8 | 27.88 ms | 13.20 ms | 1.98 ms |
| Baseline `382cc88`, host quiet, 1 | 9 | 25.40 ms | 14.62 ms | 2.77 ms |
| Baseline `382cc88`, host quiet, 2 | 11 | 9.09 ms | 13.58 ms | 2.25 ms |

The tail tracks the load on the host, not Hangar: with other projects' builds and clusters pushing the laptop's load average to 40 to 140, no version met the target, and with the load average near 10, both did. The first baseline run, at a load average of 45, had 1,600 requests time out after 30 seconds; no later run had a failed request.

The database work per download fell. A `HEAD /resolve` with a Token ran 14 queries at `382cc88` and runs 8 now (`TestHubDownloadQueries`):

| CPU per request, 60-second runs | Postgres | Each web replica |
|---|---|---|
| Baseline, host busy (3 runs) | 2.69 to 3.02 ms | 1.59 to 1.71 ms |
| Final, host busy (4 runs) | 1.77 to 1.91 ms | 1.32 to 1.46 ms |
| Baseline, host quiet (2 runs) | 1.28 to 1.78 ms | 0.96 to 1.19 ms |
| Final, host quiet (1 run) | 1.14 ms | 0.96 ms |

### Model info

100 requests per second for 2 minutes, half `/api/models/{repo}` and half `/revision/main`: p99 62.73, 29.40 and 19.30 ms with the host busy (load average 34 to 44), and 12.52 ms with it quiet. No failed requests.

### Tree listing

Four clients listing all 100,000 files of `many-files` for 2 minutes, 101 pages each: p95 6.45 and 6.79 s with the host busy, 6.15 s with it quiet. Every listing returned every file.

### Clone

Four clients cloning `many-commits` (4.4 MB pack) for 3 minutes, in five runs: p95 995.59 ms, 1.13 s and 829.95 ms with the host busy, 599 ms with it quiet. The fifth run, with the host busy, is the one in [Clone errors](#clone-errors); its p95 was not kept with the results, so it is not listed here or in the summary.

#### Clone errors

One busy run had 2 of 904 pack responses end early: k6 reported `unexpected EOF`, and the replica logged `git upload-pack ended early` with `context canceled`, which means the connection from Traefik closed first. That run failed its `http_req_failed` threshold (0.11% against 0.1%). The other four clone runs, the ones listed in [Clone](#clone), had none in about 3,900 clones, and nothing in Hangar's logs explains the closed connections. Treat it as unexplained until it happens on a quiet host.

During clones, a replica's memory peaked at 285 and 496 MiB, mostly the `git upload-pack` and `pack-objects` processes in its cgroup, under the 1 GiB limit. The 256 MiB target covers uploads and downloads of files, not git clones.

### Uploads

Four concurrent 1 GiB uploads through the native upload API (16 MiB parts, proxied to MinIO): every plan, part, completion and Commit succeeded, 1 minute 32 seconds per object on average. Each replica's cgroup peaked at 66 and 81 MiB (of which about 20 MiB was the Go heap and the rest page cache and `/tmp` files), and stayed between 35 and 66 MiB through the run.

### Downloads

Four clients downloading a 1 GiB file through `/resolve` for 2 minutes, bodies discarded: 140 downloads, 140 GiB, 1.2 GB/s in total. Each replica peaked at 104 and 115 MiB, and used 42 CPU-seconds in 122 seconds, 0.35 of its 2 CPUs. Traefik used 91 CPU-seconds, more than either replica, so the replicas were not what limited the throughput.

### Start to ready

Five times, a stopped web replica was started with `docker start` while a container on the Compose network polled its `/readyz` every 50 ms: 1.61, 0.94, 0.85, 1.41 and 1.50 seconds to the first `200`. The times include starting the polling container, so they overstate the replica's own start.

### Dataset Viewer rows

`viewer_rows.js` against a 10 million row Parquet Split (184 MB, three columns), 50 requests per second for 2 minutes, 100 rows from a random offset each: p99 27.77 and 16.11 ms, median 9.28 and 8.54 ms, every response the requested rows. The in-process load test (`go test -tags load -run TestLoadRows ./internal/datasets/viewer`) measured p99 1.89 ms on the same machine.

### Sign-in burst

Not one of the targets, but found while measuring memory: 40 parallel failed sign-ins (`POST /api/hangar/v1/auth/login`) took the baseline's replicas to their 1 GiB limit, and the kernel killed one. Each password check holds 64 MiB (argon2id), and nothing limited how many ran at once. With password hashing limited to two at a time per process (one on a 2 CPU replica), the same burst finished in 5 seconds and each replica peaked at 209 MiB.

## Deviations

- **One machine.** k6 ran on the same host as Hangar, in a container limited to 2 CPUs, not on a second machine. The Docker VM had 6 vCPUs for the two replicas (4 CPUs of limits), Postgres, MinIO, Traefik, the worker and k6.
- **A shared host.** Other projects' containers, builds and a kind cluster ran on the laptop and in the same Docker VM throughout. The laptop's load average ranged from 8 to 144 and the VM's from 2 to 8. Each result above gives the load it ran under; scenarios were repeated to separate noise from regressions.
- **Traefik's port.** Traefik listened on 18480 inside its container, so k6 could share its network namespace and follow the `Link` headers Hangar writes with its public URL.
- **50 GiB transfers.** The laptop's disk had too little room for a 50 GiB upload, so uploads were 4 × 1 GiB. Downloads repeated a 1 GiB file to 140 GiB in total.
- **Upload Repos.** `upload.js` commits each VU's file to its own Repo. With all four on one branch, a Commit could get 409 when its bounded retries ran out, which is the specified behaviour for Commits without a parent Commit, not what this scenario measures.
- **Rows dataset.** `seed.sh` does not create the 10 million row dataset; it was generated with PyArrow (columns `n` int64, `s` string, `f` float64) and pushed with git LFS.
- **Not measured.** 100,000 Repos per Instance, and the `/filter` target in the Dataset Viewer chapter, which only the in-process load test covers.
