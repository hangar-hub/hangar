import { describe, expect, it } from "vitest";
import { fromForm, toForm, validateForm } from "./card";

const imdb = {
  pretty_name: "IMDb mini",
  license: "other",
  task_categories: ["text-classification"],
  language: ["en"],
  size_categories: ["1K<n<10K"],
  configs: [
    {
      config_name: "default",
      data_files: [
        { split: "train", path: "data/train.csv" },
        { split: "test", path: "data/test.csv" },
      ],
    },
  ],
};

describe("dataset card editor", () => {
  it("reads the common fields", () => {
    const f = toForm(imdb);
    expect(f.prettyName).toBe("IMDb mini");
    expect(f.languages).toEqual(["en"]);
    expect(f.configs).toEqual([
      {
        name: "default",
        isDefault: false,
        dataDir: "",
        splits: [
          { split: "train", path: "data/train.csv" },
          { split: "test", path: "data/test.csv" },
        ],
      },
    ]);
    expect(
      toForm({ language: "fr", configs: [{ config_name: "x", data_files: "a.csv" }] }).configs[0]
        ?.splits,
    ).toEqual([{ split: "train", path: "a.csv" }]);
  });

  it("writes valid YAML values back and keeps unknown keys", () => {
    const f = toForm(imdb);
    f.prettyName = "";
    f.configs[0]?.splits.push({ split: "train", path: "data/extra.csv" });
    const out = fromForm(imdb, f);
    expect(out.size_categories).toEqual(["1K<n<10K"]);
    expect("pretty_name" in out).toBe(false);
    expect(out.configs).toEqual([
      {
        config_name: "default",
        data_files: [
          { split: "train", path: ["data/train.csv", "data/extra.csv"] },
          { split: "test", path: "data/test.csv" },
        ],
      },
    ]);
  });

  it("reports invalid Configs and languages", () => {
    const errors = validateForm({
      prettyName: "",
      licence: "",
      taskCategories: [],
      languages: ["en", "English"],
      configs: [
        { name: "a b", isDefault: true, dataDir: "", splits: [] },
        { name: "x", isDefault: true, dataDir: "", splits: [{ split: "", path: "x" }] },
        { name: "x", isDefault: false, dataDir: "d", splits: [] },
      ],
    });
    expect(errors).toEqual([
      "a b: Config names use letters, digits, '-', '_' and '.'.",
      "a b: add a data folder or at least one Split with its files.",
      "x: every Split needs a name and a file path or pattern.",
      "x: another Config has this name.",
      "Only one Config can be the default.",
      "English is not a language code such as en or pt-BR.",
    ]);
    expect(validateForm(toForm(imdb))).toEqual([]);
  });
});
