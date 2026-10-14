// HEAD and GET on /resolve, the calls every Hugging Face client makes for
// every file. Target (02-architecture.md): HEAD p99 under 50 ms at 200
// requests per second per web replica, excluding upstream Pulls.
//
//   k6 run -e HANGAR_URL=... -e HANGAR_TOKEN=... -e REPLICAS=2 loadtest/resolve.js
import http from "k6/http";
import { check } from "k6";
import { BASE, NS, auth, escapePath } from "./lib/config.js";

const REPLICAS = Number(__ENV.REPLICAS || 1);
const REPO = __ENV.RESOLVE_REPO || `${NS}/resolve`;
const FILES = (__ENV.RESOLVE_FILES || "config.json,tokenizer.json,model.safetensors,README.md").split(",");

export const options = {
  scenarios: {
    resolve: {
      executor: "constant-arrival-rate",
      rate: 200 * REPLICAS,
      timeUnit: "1s",
      duration: __ENV.DURATION || "2m",
      preAllocatedVUs: 50 * REPLICAS,
      maxVUs: 400 * REPLICAS,
    },
  },
  thresholds: {
    "http_req_duration{method:HEAD}": ["p(99)<50"],
    "http_req_failed": ["rate<0.001"],
  },
};

export default function () {
  const file = FILES[Math.floor(Math.random() * FILES.length)];
  const url = `${BASE}/${REPO}/resolve/main/${escapePath(file)}`;
  // Four HEADs for every GET, as from_pretrained checks files before it downloads.
  if (Math.random() < 0.8) {
    const r = http.head(url, Object.assign(auth(), { redirects: 0, tags: { name: "HEAD resolve" } }));
    check(r, { "HEAD 200 or 302": (res) => res.status === 200 || res.status === 302 });
  } else {
    const r = http.get(url, Object.assign(auth({ Range: "bytes=0-65535" }), { tags: { name: "GET resolve" } }));
    check(r, { "GET 200 or 206": (res) => res.status === 200 || res.status === 206 });
  }
}
