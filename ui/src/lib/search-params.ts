/**
 * Query strings as plain strings, so shared URLs read like the Hub's
 * (`?format=gguf&sort=likes`) rather than JSON.
 */
export function parseSearch(search: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of new URLSearchParams(search.startsWith("?") ? search.slice(1) : search)) {
    out[k] = v;
  }
  return out;
}

export function stringifySearch(search: Record<string, unknown>): string {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(search)) {
    if (v === undefined || v === null || v === "") continue;
    params.set(k, Array.isArray(v) ? v.join(",") : String(v));
  }
  const s = params.toString();
  return s ? `?${s}` : "";
}
