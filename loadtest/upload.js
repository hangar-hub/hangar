// Concurrent 1 GiB uploads through the native upload API, as `hangar push`
// and the web UI send them: plan, part PUTs (presigned or through Hangar),
// complete, commit. Watch each web replica's memory while it runs; the
// target (02-architecture.md) is flat and under 256 MiB per replica.
//
//   k6 run -e HANGAR_URL=... -e HANGAR_TOKEN=... -e VUS=4 loadtest/upload.js
import http from "k6/http";
import crypto from "k6/crypto";
import { check, fail } from "k6";
import { Trend } from "k6/metrics";
import { BASE, NS, auth } from "./lib/config.js";
import { eachChunk, newBase, part } from "./lib/payload.js";

const SIZE = Number(__ENV.UPLOAD_SIZE || 1 << 30);
const VUS = Number(__ENV.VUS || 4);
// Each VU commits to its own Repo, <UPLOAD_REPO>-<vu>, because this
// scenario measures uploads, not ref contention: concurrent Commits to one
// branch wait and retry (04-git.md "Server-side Commits").
const REPO = __ENV.UPLOAD_REPO || `${NS}/uploads`;
const repoOf = (vu) => `${REPO}-${vu}`;
const uploadTime = new Trend("upload_object_duration", true);

export const options = {
  scenarios: {
    uploads: { executor: "per-vu-iterations", vus: VUS, iterations: Number(__ENV.ITERATIONS || 1), maxDuration: "1h" },
  },
  thresholds: { http_req_failed: ["rate<0.001"], checks: ["rate==1"] },
};

const json = (extra) => auth(Object.assign({ "Content-Type": "application/json" }, extra));

export function setup() {
  // 409 means the Repo is there from an earlier run, so it is not a failed request.
  const params = Object.assign(json(), { responseCallback: http.expectedStatuses(201, 409) });
  for (let vu = 1; vu <= VUS; vu++) {
    const [ns, name] = repoOf(vu).split("/");
    const r = http.post(`${BASE}/api/hangar/v1/repos`, JSON.stringify({ type: "model", namespace: ns, name }), params);
    if (r.status !== 201 && r.status !== 409) fail(`create ${repoOf(vu)}: ${r.status} ${r.body}`);
  }
  // Names this run in every chunk, so no object matches one stored before.
  return { run: Date.now() };
}

export default function (data) {
  const repo = repoOf(__VU);
  const path = `blobs/vu${__VU}-iter${__ITER}-${Date.now()}.bin`;
  const base = newBase(__VU);
  const hasher = crypto.createHash("sha256");
  eachChunk(base, data.run, __VU, __ITER, SIZE, (c) => hasher.update(c.slice().buffer));
  const sha256 = hasher.digest("hex");

  const start = Date.now();
  const plan = http.post(`${BASE}/api/hangar/v1/repos/models/${repo}/uploads`,
    JSON.stringify({ files: [{ path, size: SIZE, sha256 }] }), json());
  check(plan, { "plan 200": (r) => r.status === 200 }) || fail(`plan: ${plan.status} ${plan.body}`);
  const item = plan.json("files.0");
  if (item.action === "upload") {
    const partSize = item.part_size;
    const parts = Math.ceil(SIZE / partSize);
    const done = [];
    for (let n = 1; n <= parts; n++) {
      const params = item.presigned ? { headers: {} } : auth({ "Content-Type": "application/octet-stream" });
      params.tags = { name: "upload part" };
      const r = http.put(item.part_urls[n - 1], part(base, data.run, __VU, __ITER, n, partSize, SIZE), params);
      check(r, { "part 200": (res) => res.status === 200 }) || fail(`part ${n}: ${r.status}`);
      done.push({ number: n, etag: r.headers.Etag || r.json("etag") });
    }
    const c = http.post(`${BASE}/api/hangar/v1/repos/models/${repo}/uploads/${item.upload_id}/complete`, JSON.stringify({ parts: done }), json());
    check(c, { "complete 200": (r) => r.status === 200 && r.json("state") === "verified" });
  }
  const commit = http.post(`${BASE}/api/hangar/v1/repos/models/${repo}/commits`, JSON.stringify({
    summary: `Load test upload ${path}`,
    operations: [{ op: "add_lfs", path, sha256, size: SIZE }],
  }), json());
  check(commit, { "commit 201": (r) => r.status === 201 });
  uploadTime.add(Date.now() - start);
}
