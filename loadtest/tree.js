// Lists every file of a 100,000-file Repo through the Hub API, following
// Link headers, as list_repo_tree(recursive=True) and snapshot_download do.
import http from "k6/http";
import { check } from "k6";
import { Trend } from "k6/metrics";
import { BASE, NS, auth } from "./lib/config.js";

const REPO = __ENV.TREE_REPO || `${NS}/many-files`;
const EXPECT = Number(__ENV.TREE_FILES || 100000);
const fullListing = new Trend("tree_full_listing_duration", true);

export const options = {
  scenarios: { tree: { executor: "constant-vus", vus: Number(__ENV.VUS || 4), duration: __ENV.DURATION || "2m" } },
  thresholds: { tree_full_listing_duration: ["p(95)<10000"], http_req_failed: ["rate<0.001"] },
};

export default function () {
  const start = Date.now();
  let url = `${BASE}/api/models/${REPO}/tree/main?recursive=true&expand=false`;
  let files = 0;
  while (url) {
    const r = http.get(url, Object.assign(auth(), { tags: { name: "tree page" } }));
    if (!check(r, { "200": (res) => res.status === 200 })) return;
    files += r.json().filter((e) => e.type === "file").length;
    const link = r.headers.Link || "";
    const m = link.match(/<([^>]+)>;\s*rel="next"/);
    url = m ? m[1] : null;
  }
  fullListing.add(Date.now() - start);
  check(files, { "every file listed": (n) => n >= EXPECT });
}
