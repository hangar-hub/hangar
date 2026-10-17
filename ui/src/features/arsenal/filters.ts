import type { RepoKind } from "@/lib/repo";

/**
 * A Models or Datasets list's filter state, as it appears in the query
 * string. The kind of Repo is the page, not a filter, so the two lists
 * never mix.
 */
export type ListSearch = {
  q?: string;
  ns?: string;
  format?: string;
  task?: string;
  library?: string;
  quant?: string;
  params?: string;
  context_min?: string;
  size_max?: string;
  licence?: string;
  mirror?: string;
  modality?: string;
  language?: string;
  size_category?: string;
  sort?: string;
  view?: string;
};

/** Parameter count buckets, as ranges for params_min and params_max. */
export const paramBuckets: { id: string; label: string; min?: number; max?: number }[] = [
  { id: "lt1b", label: "Under 1B", max: 1e9 },
  { id: "1-3b", label: "1B to 3B", min: 1e9, max: 3e9 },
  { id: "3-8b", label: "3B to 8B", min: 3e9, max: 8e9 },
  { id: "8-15b", label: "8B to 15B", min: 8e9, max: 15e9 },
  { id: "15-40b", label: "15B to 40B", min: 15e9, max: 40e9 },
  { id: "gt40b", label: "Over 40B", min: 40e9 },
];

export const contextOptions = [
  { value: String(4096), label: "4K or more" },
  { value: String(8192), label: "8K or more" },
  { value: String(32768), label: "32K or more" },
  { value: String(131072), label: "128K or more" },
];

const GiB = 1024 ** 3;
export const sizeOptions = [
  { value: String(GiB), label: "Up to 1 GiB" },
  { value: String(5 * GiB), label: "Up to 5 GiB" },
  { value: String(20 * GiB), label: "Up to 20 GiB" },
  { value: String(50 * GiB), label: "Up to 50 GiB" },
];

export const sortOptions = [
  { value: "updated", label: "Recently updated" },
  { value: "name", label: "Name" },
  { value: "size", label: "Size" },
  { value: "parameters", label: "Parameters", modelsOnly: true },
  { value: "downloads", label: "Downloads" },
  { value: "likes", label: "Likes" },
] as const;

/** The sort options that apply to a kind of Repo. */
export function sortOptionsFor(kind: RepoKind) {
  return sortOptions.filter((o) => kind === "model" || !("modelsOnly" in o));
}

/** The facet groups shown in the sidebar, with the query parameter each sets. */
export const facetGroups: {
  facet: string;
  param: keyof ListSearch;
  label: string;
  datasetsOnly?: boolean;
  modelsOnly?: boolean;
}[] = [
  { facet: "format", param: "format", label: "Format" },
  { facet: "task", param: "task", label: "Task" },
  { facet: "library", param: "library", label: "Library", modelsOnly: true },
  { facet: "quant", param: "quant", label: "Quantisation", modelsOnly: true },
  { facet: "modality", param: "modality", label: "Modality", datasetsOnly: true },
  { facet: "language", param: "language", label: "Language", datasetsOnly: true },
  { facet: "size_category", param: "size_category", label: "Rows", datasetsOnly: true },
  { facet: "licence", param: "licence", label: "Licence" },
  { facet: "namespace", param: "ns", label: "Namespace" },
];

/** The facet groups that apply to a kind of Repo. */
export function facetGroupsFor(kind: RepoKind) {
  return facetGroups.filter((g) => (kind === "model" ? !g.datasetsOnly : !g.modelsOnly));
}

export type ListQuery = {
  type?: "models" | "datasets";
  q?: string;
  ns?: string;
  format?: string[];
  task?: string;
  library?: string;
  quant?: string;
  params_min?: number;
  params_max?: number;
  context_min?: number;
  size_max?: number;
  licence?: string;
  mirror?: boolean;
  modality?: string;
  language?: string;
  size_category?: string;
  sort?: "updated" | "name" | "size" | "parameters" | "downloads" | "likes";
};

function num(v: string | undefined): number | undefined {
  if (v === undefined || v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
}

/**
 * Converts the query string state into GET /repos parameters for one kind
 * of Repo. Filters that do not apply to the kind, such as parameters on
 * the Datasets list, are left out.
 */
export function toListQuery(s: ListSearch, kind: RepoKind): ListQuery {
  const models = kind === "model";
  const bucket = models ? paramBuckets.find((b) => b.id === s.params) : undefined;
  const sort = sortOptionsFor(kind).find((o) => o.value === s.sort)?.value;
  const only = (forModels: boolean, v: string | undefined) =>
    forModels === models ? v || undefined : undefined;
  return {
    type: models ? "models" : "datasets",
    q: s.q || undefined,
    ns: s.ns || undefined,
    format: s.format ? s.format.split(",").filter(Boolean) : undefined,
    task: s.task || undefined,
    library: only(true, s.library),
    quant: only(true, s.quant),
    params_min: bucket?.min,
    params_max: bucket?.max,
    context_min: models ? num(s.context_min) : undefined,
    size_max: num(s.size_max),
    licence: s.licence || undefined,
    mirror: s.mirror === "true" ? true : s.mirror === "false" ? false : undefined,
    modality: only(false, s.modality),
    language: only(false, s.language),
    size_category: only(false, s.size_category),
    sort,
  };
}

/** Toggles one value in a comma-separated list. */
export function toggleListValue(list: string | undefined, value: string): string | undefined {
  const values = new Set((list ?? "").split(",").filter(Boolean));
  if (values.has(value)) values.delete(value);
  else values.add(value);
  return values.size ? [...values].join(",") : undefined;
}

/** How many filters are set, for the phone filter button. */
export function activeFilterCount(s: ListSearch, kind: RepoKind): number {
  const q = toListQuery(s, kind);
  const keys: (keyof ListSearch)[] = [
    "ns",
    "format",
    "task",
    "library",
    "quant",
    "params",
    "context_min",
    "size_max",
    "licence",
    "mirror",
    "modality",
    "language",
    "size_category",
  ];
  const applies: Partial<Record<keyof ListSearch, unknown>> = {
    ...s,
    library: q.library,
    quant: q.quant,
    params: q.params_min ?? q.params_max,
    context_min: q.context_min,
    modality: q.modality,
    language: q.language,
    size_category: q.size_category,
  };
  return keys.filter((k) => applies[k] !== undefined && applies[k] !== "").length;
}

/**
 * Where an old `/arsenal` link goes now: the Datasets list for
 * `type=datasets`, the Models list otherwise, with the other query
 * parameters kept.
 */
export function arsenalRedirect(search: Record<string, unknown>): {
  to: "/models" | "/datasets";
  search: Record<string, string>;
} {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(search)) {
    if (k === "type" || v === undefined || v === null || v === "") continue;
    out[k] = String(v);
  }
  return { to: search.type === "datasets" ? "/datasets" : "/models", search: out };
}
