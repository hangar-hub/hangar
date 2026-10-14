// model_info: GET /api/models/{repo} and /api/models/{repo}/revision/main,
// which every client calls first.
import http from "k6/http";
import { check } from "k6";
import { BASE, NS, auth } from "./lib/config.js";

const REPO = __ENV.INFO_REPO || `${NS}/resolve`;

export const options = {
  scenarios: {
    model_info: { executor: "constant-arrival-rate", rate: Number(__ENV.RATE || 100), timeUnit: "1s",
      duration: __ENV.DURATION || "2m", preAllocatedVUs: 50, maxVUs: 300 },
  },
  thresholds: { http_req_duration: ["p(99)<100"], http_req_failed: ["rate<0.001"] },
};

export default function () {
  const path = Math.random() < 0.5 ? `/api/models/${REPO}` : `/api/models/${REPO}/revision/main`;
  const r = http.get(`${BASE}${path}`, Object.assign(auth(), { tags: { name: "model_info" } }));
  check(r, { "200": (res) => res.status === 200, "has sha": (res) => !!res.json("sha") });
}
