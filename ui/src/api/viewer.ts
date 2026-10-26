import { ApiError } from "@/lib/problem";

/**
 * A client for the Dataset Viewer API (14-datasets.md, DATA-4), which
 * follows the Hub's datasets-server shapes rather than the native API.
 */

export type FeatureType = {
  _type?: string;
  dtype?: string;
  names?: string[];
  feature?: FeatureType;
  [key: string]: unknown;
};

export type Feature = { feature_idx: number; name: string; type: FeatureType };
export type ViewerRow = {
  row_idx: number;
  row: Record<string, unknown>;
  truncated_cells: string[];
};

export type RowsResponse = {
  features: Feature[];
  rows: ViewerRow[];
  num_rows_total: number;
  num_rows_per_page: number;
  partial: boolean;
};

export type ColumnStatistics = {
  column_name: string;
  column_type: string;
  column_statistics: {
    nan_count?: number;
    nan_proportion?: number;
    min?: number;
    max?: number;
    mean?: number;
    median?: number;
    std?: number;
    histogram?: { hist: number[]; bin_edges: number[] };
    frequencies?: Record<string, number>;
    n_unique?: number;
    no_label_count?: number;
  };
};

export type StatisticsResponse = {
  num_examples: number;
  statistics: ColumnStatistics[];
  partial: boolean;
};

export type SizeResponse = {
  size: {
    splits: {
      dataset: string;
      config: string;
      split: string;
      num_bytes_parquet_files: number;
      num_bytes_memory: number;
      num_rows: number;
      num_columns: number;
    }[];
  };
  partial: boolean;
};

const base = "/datasets-server";

async function get<T>(
  endpoint: string,
  params: Record<string, string | number | undefined>,
): Promise<T> {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") q.set(k, String(v));
  const res = await fetch(`${base}/${endpoint}?${q}`, { credentials: "same-origin" });
  const body = (await res.json().catch(() => ({}))) as { error?: string };
  if (!res.ok) {
    const code = res.headers.get("X-Error-Code") ?? "";
    const notReady = code === "ResponseNotReady" || /not ready/i.test(body.error ?? "");
    throw new ViewerError(
      res.status,
      notReady
        ? "Preparing preview. Hangar is building this dataset's Parquet export."
        : body.error,
      notReady,
    );
  }
  return body as T;
}

export class ViewerError extends ApiError {
  readonly notReady: boolean;
  constructor(status: number, message: string | undefined, notReady: boolean) {
    super(status, undefined, message);
    this.notReady = notReady;
  }
}

export type RowsQuery = {
  dataset: string;
  config: string;
  split: string;
  offset: number;
  length: number;
  query?: string;
  where?: string;
  orderby?: string;
};

/** Rows for the table: plain paging, full-text search, or filter and sort. */
export function fetchRows(q: RowsQuery): Promise<RowsResponse> {
  const common = {
    dataset: q.dataset,
    config: q.config,
    split: q.split,
    offset: q.offset,
    length: q.length,
  };
  if (q.query) return get("search", { ...common, query: q.query });
  if (q.where || q.orderby) return get("filter", { ...common, where: q.where, orderby: q.orderby });
  return get("rows", common);
}

export function fetchStatistics(
  dataset: string,
  config: string,
  split: string,
): Promise<StatisticsResponse> {
  return get("statistics", { dataset, config, split });
}

export function fetchSize(dataset: string, config: string): Promise<SizeResponse> {
  return get("size", { dataset, config });
}

/** True for numeric feature types, which take bare literals in `where`. */
export function isNumericFeature(t: FeatureType | undefined): boolean {
  if (!t) return false;
  if (t._type === "ClassLabel") return true;
  return typeof t.dtype === "string" && /^(u?int\d*|float\d*|double|bool)$/.test(t.dtype);
}
