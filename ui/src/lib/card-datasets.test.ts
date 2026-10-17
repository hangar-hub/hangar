import { describe, expect, it } from "vitest";
import { cardDatasets, isDatasetId, withCardDatasets } from "./card-datasets";
import { parseCardData, splitFrontMatter } from "./frontmatter";

describe("a model card's datasets metadata", () => {
  it("reads a list or a single ID, without repeats or other values", () => {
    expect(cardDatasets({ datasets: ["ada/imdb-mini", "imdb", "imdb", 3, " "] })).toEqual([
      "ada/imdb-mini",
      "imdb",
    ]);
    expect(cardDatasets({ datasets: "imdb" })).toEqual(["imdb"]);
    expect(cardDatasets({})).toEqual([]);
  });

  it("accepts the IDs the Hub accepts", () => {
    expect(isDatasetId("ada/imdb-mini")).toBe(true);
    expect(isDatasetId("imdb")).toBe(true);
    expect(isDatasetId("datasets/ada/imdb-mini")).toBe(false);
    expect(isDatasetId("a/b/c")).toBe(false);
    expect(isDatasetId("ada/")).toBe(false);
    expect(isDatasetId("ada/imdb mini")).toBe(false);
  });

  it("writes the list as YAML and keeps the other keys and the body", () => {
    const card = "---\nlicense: mit\ndatasets: imdb\ntags:\n  - bert\n---\n\n# Model\n\nText.\n";
    const next = withCardDatasets(card, ["ada/imdb-mini", "imdb"]);
    expect(next).toBe(
      "---\nlicense: mit\ndatasets:\n  - ada/imdb-mini\n  - imdb\ntags:\n  - bert\n---\n\n# Model\n\nText.\n",
    );
    expect(parseCardData(splitFrontMatter(next).yaml).datasets).toEqual(["ada/imdb-mini", "imdb"]);
  });

  it("removes the key when no datasets are left, and starts a card when there is none", () => {
    expect(withCardDatasets("---\nlicense: mit\ndatasets: [imdb]\n---\n\n# M\n", [])).toBe(
      "---\nlicense: mit\n---\n\n# M\n",
    );
    expect(withCardDatasets("---\ndatasets: [imdb]\n---\n\n# M\n", [])).toBe("# M\n");
    expect(withCardDatasets(null, ["ada/imdb-mini"])).toBe(
      "---\ndatasets:\n  - ada/imdb-mini\n---\n\n",
    );
  });

  it("refuses to rewrite a card whose YAML is not valid", () => {
    expect(() => withCardDatasets("---\n- not\n- a mapping\n---\n# M\n", ["imdb"])).toThrow(
      /mapping/,
    );
  });
});
