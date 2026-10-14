/** The Arsenal's filter state, as it appears in the query string. */
export type ArsenalSearch = {
  type?: string;
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
  { value: "parameters", label: "Parameters" },
  { value: "downloads", label: "Downloads" },
  { value: "likes", label: "Likes" },
] as const;

/** The facet groups shown in the sidebar, with the query parameter each sets. */
export const facetGroups: {
  facet: string;
  param: keyof ArsenalSearch;
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

/** Converts the query string state into GET /repos parameters. */
export function toListQuery(s: ArsenalSearch): ListQuery {
  const bucket = paramBuckets.find((b) => b.id === s.params);
  const sort = sortOptions.find((o) => o.value === s.sort)?.value;
  return {
    type: s.type === "models" || s.type === "datasets" ? s.type : undefined,
    q: s.q || undefined,
    ns: s.ns || undefined,
    format: s.format ? s.format.split(",").filter(Boolean) : undefined,
    task: s.task || undefined,
    library: s.library || undefined,
    quant: s.quant || undefined,
    params_min: bucket?.min,
    params_max: bucket?.max,
    context_min: num(s.context_min),
    size_max: num(s.size_max),
    licence: s.licence || undefined,
    mirror: s.mirror === "true" ? true : s.mirror === "false" ? false : undefined,
    modality: s.modality || undefined,
    language: s.language || undefined,
    size_category: s.size_category || undefined,
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
export function activeFilterCount(s: ArsenalSearch): number {
  const keys: (keyof ArsenalSearch)[] = [
    "type",
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
  return keys.filter((k) => s[k]).length;
}
