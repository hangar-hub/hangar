// A full `git clone` of a 10,000-Commit Repo over smart HTTP (protocol v0):
// the ref advertisement, then one upload-pack request that wants HEAD with
// no haves. Measures the time to receive the whole pack.
import http from "k6/http";
import { check } from "k6";
import { Trend, Counter } from "k6/metrics";
import encoding from "k6/encoding";
import { BASE, NS, TOKEN } from "./lib/config.js";

const REPO = __ENV.CLONE_REPO || `${NS}/many-commits`;
const USER = __ENV.HANGAR_USERNAME || "loadtest";
const cloneTime = new Trend("git_clone_duration", true);
const packBytes = new Counter("git_pack_bytes");

export const options = {
  scenarios: { clone: { executor: "constant-vus", vus: Number(__ENV.VUS || 4), duration: __ENV.DURATION || "3m" } },
  thresholds: { git_clone_duration: ["p(95)<30000"], http_req_failed: ["rate<0.001"] },
};

function headers(extra) {
  const h = Object.assign({ "User-Agent": "git/2.47.0" }, extra);
  if (TOKEN) h.Authorization = `Basic ${encoding.b64encode(`${USER}:${TOKEN}`)}`;
  return h;
}

// pkt formats one pkt-line.
function pkt(s) {
  return (s.length + 4).toString(16).padStart(4, "0") + s;
}

export default function () {
  const start = Date.now();
  const refs = http.get(`${BASE}/${REPO}.git/info/refs?service=git-upload-pack`, { headers: headers(), tags: { name: "info/refs" } });
  if (!check(refs, { "refs 200": (r) => r.status === 200 })) return;
  const m = refs.body.match(/([0-9a-f]{40}) HEAD/);
  if (!check(m, { "HEAD advertised": (x) => !!x })) return;
  const body = pkt(`want ${m[1]} multi_ack_detailed side-band-64k ofs-delta agent=git/2.47.0\n`) + "0000" + pkt("done\n");
  const pack = http.post(`${BASE}/${REPO}.git/git-upload-pack`, body, {
    headers: headers({ "Content-Type": "application/x-git-upload-pack-request", Accept: "application/x-git-upload-pack-result" }),
    responseType: "binary", tags: { name: "git-upload-pack" }, timeout: "10m",
  });
  if (!check(pack, { "pack 200": (r) => r.status === 200 })) return;
  packBytes.add(pack.body.byteLength);
  cloneTime.add(Date.now() - start);
}
