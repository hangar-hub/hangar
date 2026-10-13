import { describe, expect, it } from "vitest";
import {
  buildOrderBy,
  buildWhere,
  conditionSql,
  literal,
  nextSort,
  parseOrderBy,
  quoteColumn,
} from "./where";

describe("filter bar SQL", () => {
  it("quotes columns and literals safely", () => {
    expect(quoteColumn('we"ird')).toBe('"we""ird"');
    expect(literal("it's", false)).toBe("'it''s'");
    expect(literal("42", true)).toBe("42");
    expect(literal("42", false)).toBe("'42'");
    expect(literal("4x", true)).toBe("'4x'");
  });

  it("builds each operator", () => {
    expect(conditionSql({ column: "label", op: "=", value: "1" }, true)).toBe('"label" = 1');
    expect(conditionSql({ column: "text", op: "ILIKE", value: "great" }, false)).toBe(
      `"text" ILIKE '%great%'`,
    );
    expect(conditionSql({ column: "text", op: "LIKE", value: "A%" }, false)).toBe(
      `"text" LIKE 'A%'`,
    );
    expect(conditionSql({ column: "label", op: "IN", value: "0, 2" }, true)).toBe(
      '"label" IN (0, 2)',
    );
    expect(conditionSql({ column: "text", op: "IS NULL", value: "" }, false)).toBe(
      '"text" IS NULL',
    );
    expect(conditionSql({ column: "text", op: ">", value: "" }, false)).toBeNull();
    expect(conditionSql({ column: "", op: "=", value: "1" }, false)).toBeNull();
  });

  it("joins complete conditions with AND", () => {
    const where = buildWhere(
      [
        { column: "label", op: "=", value: "1" },
        { column: "text", op: "<>", value: "" },
        { column: "rating", op: ">=", value: "7" },
      ],
      (c) => c !== "text",
    );
    expect(where).toBe('"label" = 1 AND "rating" >= 7');
  });

  it("round-trips sorting", () => {
    expect(buildOrderBy({ column: "rating", dir: "DESC" })).toBe('"rating" DESC');
    expect(buildOrderBy(null)).toBe("");
    expect(parseOrderBy('"a""b" asc')).toEqual({ column: 'a"b', dir: "ASC" });
    expect(parseOrderBy("rating")).toBeNull();
    expect(nextSort(null, "a")).toEqual({ column: "a", dir: "ASC" });
    expect(nextSort({ column: "a", dir: "ASC" }, "a")).toEqual({ column: "a", dir: "DESC" });
    expect(nextSort({ column: "a", dir: "DESC" }, "a")).toBeNull();
    expect(nextSort({ column: "a", dir: "DESC" }, "b")).toEqual({ column: "b", dir: "ASC" });
  });
});
