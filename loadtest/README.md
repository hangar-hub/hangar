# Load tests

[k6](https://k6.io) scenarios for the scale targets in [02](../docs/spec/02-architecture.md) "Scale targets", run as [13](../docs/spec/13-testing.md) "Load tests" describes. Results go in `docs/benchmarks.md`.

| Script | Measures | Target |
|---|---|---|
| `resolve.js` | `HEAD` and `GET` on `/resolve`, four `HEAD`s per `GET`, at 200 requests per second per web replica | `HEAD` p99 under 50 ms |
| `model_info.js` | `GET /api/models/{repo}` and `/revision/main` | p99 under 100 ms |
| `tree.js` | A recursive Hub API listing of a 100,000-file Repo, following `Link` headers | a full listing in under 10 seconds at p95 |
| `upload.js` | Concurrent 1 GiB uploads through the native upload API | memory of each web replica flat and under 256 MiB |
| `git_clone.js` | A full clone of a 10,000-Commit Repo over smart HTTP | under 30 seconds at p95 |

## Reference setup

Two `web` replicas (2 vCPU, 1 GiB each), one `worker`, Postgres and MinIO on one host: the `deploy/compose/scale.yaml` example with CPU and memory limits added. Run k6 from a second machine on the same network, so the load generator does not compete for CPU.

## Run

1. Start the Instance and create a write Token for a user (Settings, Tokens).
2. Seed the Repos the scenarios read:

   ```sh
   HANGAR_URL=http://hangar.lan:8080 HANGAR_TOKEN=hgr_... HANGAR_USERNAME=loadtest loadtest/seed.sh
   ```

3. Run each scenario, with the official image or a local k6:

   ```sh
   docker run --rm -i --network host -v "$PWD/loadtest:/lt" -w /lt grafana/k6:1.4.2 run \
     -e HANGAR_URL=http://hangar.lan:8080 -e HANGAR_TOKEN=hgr_... -e HANGAR_NAMESPACE=loadtest \
     -e REPLICAS=2 resolve.js
   ```

   `upload.js` takes `VUS` (default 4) and `UPLOAD_SIZE` (default 1 GiB); watch replica memory with `docker stats` while it runs. `git_clone.js` takes `HANGAR_USERNAME` for the git Basic credentials.

Every script also takes `DURATION`. k6 exits non-zero when a threshold fails.
