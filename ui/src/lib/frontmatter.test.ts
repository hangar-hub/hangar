import { describe, expect, it } from "vitest";
import { joinFrontMatter, parseCardData, splitFrontMatter } from "./frontmatter";

describe("card front matter", () => {
  it("splits and parses YAML metadata", () => {
    const { yaml, body } = splitFrontMatter("---\nlicense: mit\ntags: [a, b]\n---\n\n# Title\n");
    expect(parseCardData(yaml)).toEqual({ license: "mit", tags: ["a", "b"] });
    expect(body).toBe("\n# Title\n");
    expect(splitFrontMatter("# No metadata")).toEqual({ yaml: "", body: "# No metadata" });
  });

  it("refuses YAML that is not a mapping and aliases", () => {
    expect(() => parseCardData("- a\n- b")).toThrow(/mapping/);
    expect(() => parseCardData("a: &x [1]\nb: *x")).toThrow();
    expect(parseCardData("")).toEqual({});
  });

  it("writes metadata back and drops empty values", () => {
    const card = joinFrontMatter(
      { pretty_name: "IMDb", license: "", language: [], task_categories: ["text-classification"] },
      "\n\n# Body\n",
    );
    expect(card).toBe(
      "---\npretty_name: IMDb\ntask_categories:\n  - text-classification\n---\n\n# Body\n",
    );
    expect(joinFrontMatter({}, "# Body")).toBe("# Body");
    expect(parseCardData(splitFrontMatter(card).yaml)).toEqual({
      pretty_name: "IMDb",
      task_categories: ["text-classification"],
    });
  });
});
