import { describe, expect, it } from "vitest";
import { parseSearch, stringifySearch } from "./search-params";

describe("query strings", () => {
  it("reads and writes plain strings", () => {
    expect(parseSearch("?format=gguf,mlx&sort=likes")).toEqual({
      format: "gguf,mlx",
      sort: "likes",
    });
    expect(stringifySearch({ format: ["gguf", "mlx"], q: "", n: 3, x: undefined })).toBe(
      "?format=gguf%2Cmlx&n=3",
    );
    expect(stringifySearch({})).toBe("");
  });
});
