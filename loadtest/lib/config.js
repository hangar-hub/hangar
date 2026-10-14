// Shared settings for the load tests. Every value comes from an environment
// variable, so the same scripts run against any Instance.
export const BASE = (__ENV.HANGAR_URL || "http://localhost:8080").replace(/\/$/, "");
export const TOKEN = __ENV.HANGAR_TOKEN || "";
export const NS = __ENV.HANGAR_NAMESPACE || "loadtest";

export function auth(extra = {}) {
  const headers = Object.assign({}, extra);
  if (TOKEN) headers.Authorization = `Bearer ${TOKEN}`;
  return { headers };
}

// Escapes each path segment, keeping slashes.
export function escapePath(p) {
  return p.split("/").map(encodeURIComponent).join("/");
}
