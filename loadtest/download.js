// Concurrent downloads of a large file through /resolve, as hf_hub_download
// sends them, with the response bodies discarded. Run it after upload.js:
// by default it downloads the largest file in <namespace>/uploads-1. Watch
// each web replica's memory while it runs; the target (02-architecture.md)
// is flat and under 256 MiB per replica, and in proxy mode the throughput
// is limited by network and storage, not CPU.
//
//   k6 run -e HANGAR_URL=... -e HANGAR_TOKEN=... -e VUS=4 loadtest/download.js
import http from "k6/http";
import { check, fail } from "k6";
import { Counter, Trend } from "k6/metrics";
import { BASE, NS, auth, escapePath } from "./lib/config.js";

const REPO = __ENV.DOWNLOAD_REPO || `${NS}/uploads-1`;
const downloadTime = new Trend("download_file_duration", true);
const downloadBytes = new Counter("download_bytes");

export const options = {
  discardResponseBodies: true,
  scenarios: {
    downloads: { executor: "constant-vus", vus: Number(__ENV.VUS || 4), duration: __ENV.DURATION || "3m" },
  },
  thresholds: { http_req_failed: ["rate<0.001"], checks: ["rate==1"] },
};

export function setup() {
  if (__ENV.DOWNLOAD_PATH) return { path: __ENV.DOWNLOAD_PATH };
  const r = http.get(`${BASE}/api/models/${REPO}/tree/main?recursive=true`, Object.assign(auth(), { responseType: "text" }));
  if (r.status !== 200) fail(`list ${REPO}: ${r.status}`);
  const files = r.json().filter((e) => e.type === "file");
  if (files.length === 0) fail(`${REPO} has no files; run upload.js first or set DOWNLOAD_PATH`);
  files.sort((a, b) => b.size - a.size);
  return { path: files[0].path };
}

export default function (data) {
  const start = Date.now();
  const r = http.get(`${BASE}/${REPO}/resolve/main/${escapePath(data.path)}`,
    Object.assign(auth(), { tags: { name: "GET resolve" }, timeout: "30m" }));
  check(r, { "200": (res) => res.status === 200 });
  const size = Number(r.headers["Content-Length"] || 0);
  downloadBytes.add(size);
  downloadTime.add(Date.now() - start);
}
