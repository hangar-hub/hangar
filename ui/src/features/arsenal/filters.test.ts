import { describe, expect, it } from "vitest";
import { activeFilterCount, arsenalRedirect, sortOptionsFor, toListQuery } from "./filters";

describe("Models and Datasets list filters", () => {
  it("always asks for one kind of Repo, so the lists never mix", () => {
    expect(toListQuery({}, "model").type).toBe("models");
    expect(toListQuery({}, "dataset").type).toBe("datasets");
  });

  it("drops filters that do not apply to the kind", () => {
    const search = {
      quant: "Q4_K_M",
      library: "transformers",
      params: "1-3b",
      context_min: "4096",
      modality: "text",
      language: "en",
      size_category: "n<1K",
      licence: "mit",
      sort: "parameters",
    };
    const models = toListQuery(search, "model");
    expect(models).toMatchObject({
      quant: "Q4_K_M",
      library: "transformers",
      params_min: 1e9,
      params_max: 3e9,
      context_min: 4096,
      licence: "mit",
      sort: "parameters",
    });
    expect(models.modality).toBeUndefined();
    expect(models.language).toBeUndefined();
    const datasets = toListQuery(search, "dataset");
    expect(datasets).toMatchObject({
      modality: "text",
      language: "en",
      size_category: "n<1K",
      licence: "mit",
    });
    expect(datasets.quant).toBeUndefined();
    expect(datasets.library).toBeUndefined();
    expect(datasets.params_min).toBeUndefined();
    expect(datasets.context_min).toBeUndefined();
    expect(datasets.sort).toBeUndefined();
    expect(activeFilterCount(search, "model")).toBe(5);
    expect(activeFilterCount(search, "dataset")).toBe(4);
  });

  it("sorts datasets by everything but parameters", () => {
    expect(sortOptionsFor("model").map((o) => o.value)).toContain("parameters");
    expect(sortOptionsFor("dataset").map((o) => o.value)).not.toContain("parameters");
  });

  it("sends old Arsenal links to the list for their type, with their other filters", () => {
    expect(arsenalRedirect({})).toEqual({ to: "/models", search: {} });
    expect(arsenalRedirect({ ns: "ada", view: "list", type: "models" })).toEqual({
      to: "/models",
      search: { ns: "ada", view: "list" },
    });
    expect(arsenalRedirect({ type: "datasets", sort: "name", q: "" })).toEqual({
      to: "/datasets",
      search: { sort: "name" },
    });
  });
});
