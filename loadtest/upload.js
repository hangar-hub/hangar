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

const SIZE = Number(__ENV.UPLOAD_SIZE || 1 << 30);
const REPO = __ENV.UPLOAD_REPO || `${NS}/uploads`;
const uploadTime = new Trend("upload_object_duration", true);

export const options = {
  scenarios: {
    uploads: { executor: "per-vu-iterations", vus: Number(__ENV.VUS || 4), iterations: Number(__ENV.ITERATIONS || 1), maxDuration: "1h" },
  },
  thresholds: { http_req_failed: ["rate<0.001"], checks: ["rate==1"] },
};

const json = (extra) => auth(Object.assign({ "Content-Type": "application/json" }, extra));

export function setup() {
  const [ns, name] = REPO.split("/");
  const r = http.post(`${BASE}/api/hangar/v1/repos`, JSON.stringify({ type: "model", namespace: ns, name }), json());
  if (r.status !== 201 && r.status !== 409) fail(`create ${REPO}: ${r.status} ${r.body}`);
}

const CHUNK = 1 << 20;

// chunk returns the bytes at chunk index i of this VU's object: a fixed
// pattern with a header that makes every chunk of every object distinct, so
// nothing is deduplicated.
function chunk(buf, iter, i) {
  const header = `vu=${__VU} iter=${iter} chunk=${i} `;
  for (let k = 0; k < header.length; k++) buf[k] = header.charCodeAt(k);
  return buf;
}

// part returns part n (1-based) of the object, built from chunks.
function part(base, iter, n, partSize) {
  const off = (n - 1) * partSize;
  const len = Math.min(partSize, SIZE - off);
  const out = new Uint8Array(len);
  for (let o = 0; o < len; o += CHUNK) {
    const c = chunk(base, iter, (off + o) / CHUNK);
    out.set(c.subarray(0, Math.min(CHUNK, len - o)), o);
  }
  return out.buffer;
}

export default function () {
  const path = `blobs/vu${__VU}-iter${__ITER}-${Date.now()}.bin`;
  const base = new Uint8Array(CHUNK);
  for (let i = 0; i < CHUNK; i++) base[i] = (i * 31 + __VU) & 0xff;
  const hasher = crypto.createHash("sha256");
  for (let off = 0; off < SIZE; off += CHUNK) {
    const c = chunk(base, __ITER, off / CHUNK);
    hasher.update(c.slice(0, Math.min(CHUNK, SIZE - off)).buffer);
  }
  const sha256 = hasher.digest("hex");

  const start = Date.now();
  const plan = http.post(`${BASE}/api/hangar/v1/repos/models/${REPO}/uploads`,
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
      const r = http.put(item.part_urls[n - 1], part(base, __ITER, n, partSize), params);
      check(r, { "part 200": (res) => res.status === 200 }) || fail(`part ${n}: ${r.status}`);
      done.push({ number: n, etag: r.headers.Etag || r.json("etag") });
    }
    const c = http.post(`${BASE}/api/hangar/v1/repos/models/${REPO}/uploads/${item.upload_id}/complete`, JSON.stringify({ parts: done }), json());
    check(c, { "complete 200": (r) => r.status === 200 && r.json("state") === "verified" });
  }
  const commit = http.post(`${BASE}/api/hangar/v1/repos/models/${REPO}/commits`, JSON.stringify({
    summary: `Load test upload ${path}`,
    operations: [{ op: "add_lfs", path, sha256, size: SIZE }],
  }), json());
  check(commit, { "commit 201": (r) => r.status === 201 });
  uploadTime.add(Date.now() - start);
}
