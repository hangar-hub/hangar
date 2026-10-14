/**
 * Builds the `where` and `orderby` parameters of the Dataset Viewer's
 * `/filter` endpoint from the filter bar (DATA-6). Columns are double
 * quoted and string literals single quoted, with quotes doubled, as the
 * Hub's viewer expects.
 */

export const operators = [
  "=",
  "<>",
  "<",
  "<=",
  ">",
  ">=",
  "LIKE",
  "ILIKE",
  "IN",
  "IS NULL",
  "IS NOT NULL",
] as const;
export type Operator = (typeof operators)[number];

export type Condition = { column: string; op: Operator; value: string };

export const operatorLabels: Record<Operator, string> = {
  "=": "equals",
  "<>": "does not equal",
  "<": "is less than",
  "<=": "is at most",
  ">": "is greater than",
  ">=": "is at least",
  LIKE: "matches (case-sensitive)",
  ILIKE: "matches",
  IN: "is one of",
  "IS NULL": "is empty",
  "IS NOT NULL": "is not empty",
};

export function quoteColumn(name: string): string {
  return `"${name.replaceAll('"', '""')}"`;
}

/** A literal: numbers stay bare for numeric columns, everything else is a quoted string. */
export function literal(value: string, numeric: boolean): string {
  const v = value.trim();
  if (numeric && v !== "" && Number.isFinite(Number(v))) return String(Number(v));
  if (/^(true|false)$/i.test(v) && numeric) return v.toLowerCase();
  return `'${value.replaceAll("'", "''")}'`;
}

/** Whether a condition needs a value. */
export function needsValue(op: Operator): boolean {
  return op !== "IS NULL" && op !== "IS NOT NULL";
}

/** One condition as SQL, or null when it is incomplete. */
export function conditionSql(c: Condition, numeric: boolean): string | null {
  if (!c.column) return null;
  const col = quoteColumn(c.column);
  if (!needsValue(c.op)) return `${col} ${c.op}`;
  if (c.value.trim() === "") return null;
  if (c.op === "IN") {
    const items = c.value
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean)
      .map((s) => literal(s, numeric));
    return items.length ? `${col} IN (${items.join(", ")})` : null;
  }
  if (c.op === "LIKE" || c.op === "ILIKE") {
    const pattern = c.value.includes("%") || c.value.includes("_") ? c.value : `%${c.value}%`;
    return `${col} ${c.op} ${literal(pattern, false)}`;
  }
  return `${col} ${c.op} ${literal(c.value, numeric)}`;
}

/** Every complete condition joined with AND. */
export function buildWhere(
  conditions: Condition[],
  isNumeric: (column: string) => boolean,
): string {
  return conditions
    .map((c) => conditionSql(c, isNumeric(c.column)))
    .filter((s): s is string => s !== null)
    .join(" AND ");
}

export type Sort = { column: string; dir: "ASC" | "DESC" } | null;

export function buildOrderBy(sort: Sort): string {
  return sort ? `${quoteColumn(sort.column)} ${sort.dir}` : "";
}

/** Reads the sort back from an orderby parameter. */
export function parseOrderBy(orderby: string | undefined): Sort {
  if (!orderby) return null;
  const m = /^"((?:[^"]|"")+)"\s+(ASC|DESC)$/i.exec(orderby.trim());
  if (!m) return null;
  return {
    column: (m[1] as string).replaceAll('""', '"'),
    dir: (m[2] as string).toUpperCase() as "ASC" | "DESC",
  };
}

/** Next sort when a column header is clicked: ascending, descending, then none. */
export function nextSort(current: Sort, column: string): Sort {
  if (!current || current.column !== column) return { column, dir: "ASC" };
  if (current.dir === "ASC") return { column, dir: "DESC" };
  return null;
}
