import type { MockState } from "./data.ts";
import type { Result } from "./handlers.ts";
import { findRepo } from "./repo-model.ts";

/**
 * A Dataset Viewer API compatible with the Hub's (14-datasets.md, DATA-4),
 * over generated rows. Errors use the Hub viewer's `{ "error": ... }` shape.
 */

type Feature = { feature_idx: number; name: string; type: Record<string, unknown> };
type Row = Record<string, unknown>;
type Split = { features: Feature[]; rows: (i: number) => Row; count: number };

const reviews = [
  "A quiet triumph, with a lead performance that stays with you.",
  "Two hours I will not get back.",
  "The score is wonderful; the script is not.",
  "Funny, sharp and far too short.",
  "I fell asleep twice, which says enough.",
];

function imdb(count: number, offset: number): Split {
  return {
    count,
    features: [
      { feature_idx: 0, name: "text", type: { dtype: "string", _type: "Value" } },
      { feature_idx: 1, name: "label", type: { names: ["neg", "pos"], _type: "ClassLabel" } },
      { feature_idx: 2, name: "rating", type: { dtype: "int64", _type: "Value" } },
      {
        feature_idx: 3,
        name: "meta",
        type: {
          source: { dtype: "string", _type: "Value" },
          tags: { feature: { dtype: "string", _type: "Value" }, _type: "List" },
        },
      },
    ],
    rows: (i) => {
      const n = i + offset;
      const label = (n * 7) % 3 === 0 ? 0 : 1;
      const text = `${reviews[n % reviews.length]} (review ${n})${n % 50 === 0 ? ` ${"Long review. ".repeat(40)}` : ""}`;
      return {
        text,
        label,
        rating: label ? 6 + (n % 5) : 1 + (n % 5),
        meta: {
          source: n % 2 ? "imdb.com" : "archive",
          tags: n % 4 ? ["film"] : ["film", "classic"],
        },
      };
    },
  };
}

function cifar(count: number): Split {
  return {
    count,
    features: [
      { feature_idx: 0, name: "img", type: { _type: "Image" } },
      {
        feature_idx: 1,
        name: "label",
        type: { names: ["cat", "dog", "ship"], _type: "ClassLabel" },
      },
    ],
    rows: (i) => ({
      img: {
        src: `/datasets-server/assets/ada/cifar-mini/--/default/train/${i}/img/image.svg`,
        height: 32,
        width: 32,
      },
      label: i % 3,
    }),
  };
}

function splitOf(dataset: string, config: string, split: string): Split | undefined {
  if (config !== "default") return undefined;
  if (dataset === "ada/imdb-mini")
    return split === "train" ? imdb(2000, 0) : split === "test" ? imdb(500, 2000) : undefined;
  if (dataset === "ada/cifar-mini" && split === "train") return cifar(300);
  return undefined;
}

const err = (status: number, error: string, code?: string): Result => ({
  status,
  body: { error },
  headers: code ? { "X-Error-Code": code } : undefined,
});

function cellValue(row: Row, col: string): unknown {
  return row[col];
}

type Pred = (row: Row) => boolean;

/** A small evaluator for the `where` clauses the mock needs; the real parser is W10's. */
export function parseWhere(where: string): Pred | null {
  const parts = where.split(/\s+(AND|OR)\s+/i);
  const preds: Pred[] = [];
  const ops: string[] = [];
  for (let i = 0; i < parts.length; i++) {
    const p = (parts[i] as string).trim();
    if (i % 2 === 1) {
      ops.push(p.toUpperCase());
      continue;
    }
    const m =
      /^"([^"]+)"\s*(=|<>|!=|<=|>=|<|>|NOT ILIKE|ILIKE|NOT LIKE|LIKE|IS NOT NULL|IS NULL|IN)\s*(.*)$/i.exec(
        p,
      );
    if (!m) return null;
    const [, col, rawOp, rawVal] = m as unknown as [string, string, string, string];
    const op = rawOp.toUpperCase();
    const lit = (v: string): unknown => {
      const t = v.trim();
      if (/^'.*'$/.test(t)) return t.slice(1, -1).replaceAll("''", "'");
      const n = Number(t);
      return Number.isNaN(n) ? t : n;
    };
    const val = lit(rawVal);
    preds.push((row) => {
      const v = cellValue(row, col);
      switch (op) {
        case "=":
          return v === val;
        case "<>":
        case "!=":
          return v !== val;
        case "<":
          return (v as number) < (val as number);
        case "<=":
          return (v as number) <= (val as number);
        case ">":
          return (v as number) > (val as number);
        case ">=":
          return (v as number) >= (val as number);
        case "IS NULL":
          return v === null || v === undefined;
        case "IS NOT NULL":
          return v !== null && v !== undefined;
        case "IN": {
          const list = rawVal
            .replace(/^\(|\)$/g, "")
            .split(",")
            .map(lit);
          return list.includes(v);
        }
        default: {
          const re = new RegExp(
            `^${String(val)
              .replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
              .replaceAll("%", ".*")
              .replaceAll("_", ".")}$`,
            op.includes("ILIKE") ? "is" : "s",
          );
          const hit = re.test(String(v));
          return op.startsWith("NOT") ? !hit : hit;
        }
      }
    });
  }
  return (row) => {
    let acc = (preds[0] as Pred)(row);
    for (let i = 1; i < preds.length; i++) {
      acc = ops[i - 1] === "OR" ? acc || (preds[i] as Pred)(row) : acc && (preds[i] as Pred)(row);
    }
    return acc;
  };
}

function rowsPage(
  s: Split,
  indices: number[],
  offset: number,
  length: number,
  total = indices.length,
) {
  return {
    features: s.features,
    rows: indices.slice(offset, offset + length).map((i) => {
      const row = s.rows(i);
      const truncated = typeof row.text === "string" && row.text.length > 300 ? ["text"] : [];
      if (truncated.length) row.text = (row.text as string).slice(0, 300);
      return { row_idx: i, row, truncated_cells: truncated };
    }),
    num_rows_total: total,
    num_rows_per_page: 100,
    partial: false,
  };
}

function stats(s: Split) {
  const all = Array.from({ length: s.count }, (_, i) => s.rows(i));
  const numeric = (vals: number[]) => {
    const sorted = [...vals].sort((a, b) => a - b);
    const mean = vals.reduce((a, v) => a + v, 0) / vals.length;
    const min = sorted[0] ?? 0;
    const max = sorted[sorted.length - 1] ?? 0;
    const bins = 10;
    const step = (max - min) / bins || 1;
    const hist = new Array<number>(bins).fill(0);
    for (const v of vals) {
      const i = Math.min(bins - 1, Math.floor((v - min) / step));
      hist[i] = (hist[i] ?? 0) + 1;
    }
    return {
      nan_count: 0,
      nan_proportion: 0,
      min,
      max,
      mean: Number(mean.toFixed(5)),
      median: sorted[Math.floor(sorted.length / 2)] ?? 0,
      std: Number(
        Math.sqrt(vals.reduce((a, v) => a + (v - mean) ** 2, 0) / vals.length).toFixed(5),
      ),
      histogram: {
        hist,
        bin_edges: Array.from({ length: bins + 1 }, (_, i) => Math.round(min + i * step)),
      },
    };
  };
  return {
    num_examples: s.count,
    partial: false,
    statistics: s.features.map((f) => {
      const t = f.type;
      if (t._type === "ClassLabel") {
        const names = t.names as string[];
        const freq: Record<string, number> = {};
        for (const r of all)
          freq[names[r[f.name] as number] as string] =
            (freq[names[r[f.name] as number] as string] ?? 0) + 1;
        return {
          column_name: f.name,
          column_type: "class_label",
          column_statistics: {
            nan_count: 0,
            nan_proportion: 0,
            no_label_count: 0,
            no_label_proportion: 0,
            n_unique: names.length,
            frequencies: freq,
          },
        };
      }
      if (t._type === "Image") {
        return {
          column_name: f.name,
          column_type: "image",
          column_statistics: numeric(all.map(() => 32)),
        };
      }
      if (t.dtype === "string") {
        return {
          column_name: f.name,
          column_type: "string_text",
          column_statistics: numeric(all.map((r) => String(r[f.name]).length)),
        };
      }
      if (t.dtype === "int64") {
        return {
          column_name: f.name,
          column_type: "int",
          column_statistics: numeric(all.map((r) => r[f.name] as number)),
        };
      }
      return { column_name: f.name, column_type: "struct", column_statistics: {} };
    }),
  };
}

function svgImage(i: number): string {
  const hue = (i * 47) % 360;
  return `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32"><rect width="32" height="32" fill="hsl(${hue} 55% 55%)"/><circle cx="16" cy="16" r="${6 + (i % 8)}" fill="hsl(${(hue + 180) % 360} 60% 35%)"/></svg>`;
}

/** Answers /datasets-server/... requests, or null for other paths. */
export function viewer(
  state: MockState,
  path: string,
  q: URLSearchParams,
): (Result & { raw?: string; type?: string }) | null {
  if (!path.startsWith("/datasets-server/")) return null;
  const endpoint = path.slice("/datasets-server/".length);
  const asset = /^assets\/.+\/(\d+)\/img\/image\.svg$/.exec(endpoint);
  if (asset) return { status: 200, raw: svgImage(Number(asset[1])), type: "image/svg+xml" };
  const dataset = q.get("dataset") ?? "";
  const [ns, name] = dataset.split("/");
  const repo = ns && name ? findRepo(state, "datasets", ns, name) : undefined;
  if (!repo)
    return err(
      404,
      "The dataset does not exist, or is not accessible without authentication (private or gated). Please check the spelling of the dataset name or retry with authentication.",
      "DatasetNotFoundError",
    );
  const config = q.get("config") ?? "";
  const splitName = q.get("split") ?? "";
  const configs = repo.configs?.configs ?? [];
  switch (endpoint) {
    case "is-valid": {
      const ready = configs.some((c) => c.splits.some((s) => s.status === "ready"));
      return {
        status: 200,
        body: { preview: ready, viewer: ready, search: ready, filter: ready, statistics: ready },
      };
    }
    case "splits":
      return {
        status: 200,
        body: {
          splits: configs.flatMap((c) =>
            c.splits.map((s) => ({ dataset, config: c.config, split: s.split })),
          ),
          pending: [],
          failed: [],
        },
      };
    case "size": {
      const splits = configs
        .filter((c) => !config || c.config === config)
        .flatMap((c) =>
          c.splits.map((s) => ({
            dataset,
            config: c.config,
            split: s.split,
            num_bytes_parquet_files: s.num_bytes_parquet ?? 0,
            num_bytes_memory: (s.num_bytes_parquet ?? 0) * 2,
            num_rows: s.num_rows ?? 0,
            num_columns: splitOf(dataset, c.config, s.split)?.features.length ?? 0,
          })),
        );
      return {
        status: 200,
        body: {
          size: {
            dataset: {
              dataset,
              num_bytes_parquet_files: splits.reduce((a, s) => a + s.num_bytes_parquet_files, 0),
              num_bytes_memory: 0,
              num_rows: splits.reduce((a, s) => a + s.num_rows, 0),
            },
            configs: [],
            splits,
          },
          pending: [],
          failed: [],
          partial: false,
        },
      };
    }
  }
  const s = splitOf(dataset, config, splitName);
  if (!s) {
    const known = configs
      .find((c) => c.config === config)
      ?.splits.find((x) => x.split === splitName);
    if (known && known.status !== "ready")
      return err(500, "The response is not ready yet. Please retry later.", "ResponseNotReady");
    return err(404, "Not found.", "ResponseNotFound");
  }
  const offset = Math.max(0, Number(q.get("offset") ?? 0));
  const length = Math.min(100, Math.max(0, Number(q.get("length") ?? 100)));
  const all = Array.from({ length: s.count }, (_, i) => i);
  switch (endpoint) {
    case "first-rows":
      return {
        status: 200,
        body: { dataset, config, split: splitName, ...rowsPage(s, all, 0, 100) },
      };
    case "rows":
      return { status: 200, body: rowsPage(s, all, offset, length) };
    case "search": {
      const terms = (q.get("query") ?? "").toLowerCase().split(/\s+/).filter(Boolean);
      const hits = all.filter((i) => {
        const row = s.rows(i);
        const text = Object.values(row)
          .filter((v) => typeof v === "string")
          .join(" ")
          .toLowerCase();
        return terms.every((t) => text.includes(t));
      });
      return { status: 200, body: rowsPage(s, hits, offset, length) };
    }
    case "filter": {
      const where = q.get("where") ?? "";
      const pred = where ? parseWhere(where) : () => true;
      if (!pred) return err(400, "Parameter 'where' is invalid", "InvalidParameter");
      let hits = all.filter((i) => pred(s.rows(i)));
      const orderby = q.get("orderby");
      if (orderby) {
        const m = /^"([^"]+)"\s*(ASC|DESC)?$/i.exec(orderby.trim());
        if (!m) return err(400, "Parameter 'orderby' is invalid", "InvalidParameter");
        const col = m[1] as string;
        const dir = (m[2] ?? "ASC").toUpperCase() === "DESC" ? -1 : 1;
        hits = [...hits].sort((a, b) => {
          const x = s.rows(a)[col] as number | string;
          const y = s.rows(b)[col] as number | string;
          return (x < y ? -1 : x > y ? 1 : 0) * dir;
        });
      }
      return { status: 200, body: rowsPage(s, hits, offset, length) };
    }
    case "statistics":
      return { status: 200, body: stats(s) };
    default:
      return err(404, "Not found.", "ResponseNotFound");
  }
}
