import { joinFrontMatter, parseCardData, splitFrontMatter } from "./frontmatter";

/**
 * A model card's `datasets:` metadata, which lists the datasets the model
 * was trained or evaluated on, as on the Hugging Face Hub. Each entry is a
 * dataset Repo ID such as `ada/imdb-mini` (no `datasets/` prefix), or a
 * single-segment ID such as `imdb` for a dataset on an Upstream.
 */

/** The dataset IDs a card's metadata lists, in order, without repeats. */
export function cardDatasets(data: Record<string, unknown>): string[] {
  const v = data.datasets;
  const list = typeof v === "string" ? [v] : Array.isArray(v) ? v : [];
  const out: string[] = [];
  for (const x of list) {
    if (typeof x !== "string") continue;
    const id = x.trim();
    if (id && !out.includes(id)) out.push(id);
  }
  return out;
}

/** True for an ID the card can list: `name` or `namespace/name`. */
export function isDatasetId(id: string): boolean {
  return /^[A-Za-z0-9][\w.-]*(\/[A-Za-z0-9][\w.-]*)?$/.test(id) && !id.startsWith("datasets/");
}

/**
 * Returns the card text with its `datasets:` metadata set to `datasets`,
 * keeping every other key, the key order and the Markdown body. An empty
 * list removes the key. Throws when the card's YAML is not valid, so a
 * broken card is never overwritten.
 */
export function withCardDatasets(card: string | null, datasets: string[]): string {
  const { yaml, body } = splitFrontMatter(card ?? "");
  const data = { ...parseCardData(yaml) };
  if (datasets.length) data.datasets = [...datasets];
  else delete data.datasets;
  // Without metadata the card is just its body, from its first line.
  return joinFrontMatter(data, yaml ? body.replace(/^\n+/, "") : body);
}
