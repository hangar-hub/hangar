import { parse, stringify } from "yaml";

/** Splits a Model card into its YAML front matter and Markdown body. */
export function splitFrontMatter(text: string): { yaml: string; body: string } {
  const m = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/.exec(text);
  if (!m) return { yaml: "", body: text };
  return { yaml: m[1] ?? "", body: text.slice(m[0].length) };
}

/**
 * Parses card metadata (`cardData`). Returns {} for a card without front
 * matter, and throws a readable error when the YAML is invalid.
 */
export function parseCardData(yaml: string): Record<string, unknown> {
  if (!yaml.trim()) return {};
  const data = parse(yaml, { maxAliasCount: 0 }) as unknown;
  if (data === null || data === undefined) return {};
  if (typeof data !== "object" || Array.isArray(data)) {
    throw new Error("The card's metadata must be a YAML mapping of keys to values.");
  }
  return data as Record<string, unknown>;
}

/** Joins metadata and a body back into a card, keeping key order. */
export function joinFrontMatter(data: Record<string, unknown>, body: string): string {
  const clean: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(data)) {
    if (v === undefined || v === "" || (Array.isArray(v) && v.length === 0)) continue;
    clean[k] = v;
  }
  if (Object.keys(clean).length === 0) return body;
  const yaml = stringify(clean, { lineWidth: 0 }).trimEnd();
  return `---\n${yaml}\n---\n\n${body.replace(/^\n+/, "")}`;
}
