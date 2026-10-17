// Dataset Viewer /rows at random offsets of a large Split, as the Viewer
// tab and the datasets-server client page through it. Target
// (14-datasets.md "Tests"): p99 under 200 ms on a 10 million row Split.
// seed.sh does not create the dataset: push one with a Split of
// ROWS_TOTAL rows (see README.md) and wait until /is-valid reports it.
//
//   k6 run -e HANGAR_URL=... -e HANGAR_TOKEN=... loadtest/viewer_rows.js
import http from "k6/http";
import { check } from "k6";
import { BASE, NS, auth } from "./lib/config.js";

const DATASET = __ENV.ROWS_DATASET || `${NS}/rows10m`;
const CONFIG = __ENV.ROWS_CONFIG || "default";
const SPLIT = __ENV.ROWS_SPLIT || "train";
const TOTAL = Number(__ENV.ROWS_TOTAL || 10000000);
const LENGTH = 100;

export const options = {
  scenarios: {
    rows: { executor: "constant-arrival-rate", rate: Number(__ENV.RATE || 50), timeUnit: "1s",
      duration: __ENV.DURATION || "2m", preAllocatedVUs: 50, maxVUs: 200 },
  },
  thresholds: { http_req_duration: ["p(99)<200"], http_req_failed: ["rate<0.001"] },
};

export default function () {
  const offset = Math.floor(Math.random() * (TOTAL - LENGTH));
  const q = `dataset=${encodeURIComponent(DATASET)}&config=${CONFIG}&split=${SPLIT}&offset=${offset}&length=${LENGTH}`;
  const r = http.get(`${BASE}/datasets-server/rows?${q}`, Object.assign(auth(), { tags: { name: "rows" } }));
  check(r, {
    "200": (res) => res.status === 200,
    "rows from the offset": (res) => res.json("rows.0.row_idx") === offset && res.json("rows").length === LENGTH,
  });
}
