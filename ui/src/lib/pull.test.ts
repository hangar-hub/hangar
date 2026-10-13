import { describe, expect, it } from "vitest";
import type { DiscoverDetail } from "@/api/types";
import { filesForQuants, isProjector, selectionBytes, toFilterParams } from "./pull";

const MiB = 1024 * 1024;
const detail: Pick<DiscoverDetail, "files" | "quants" | "configs"> = {
  files: [
    { path: "README.md", size: 1000, lfs: false, present: false },
    { path: "m.Q4_K_M.gguf", size: 400 * MiB, lfs: true, present: false },
    { path: "m.F16-00001-of-00002.gguf", size: 600 * MiB, lfs: true, present: false },
    { path: "m.F16-00002-of-00002.gguf", size: 300 * MiB, lfs: true, present: true },
    { path: "mmproj-m-f16.gguf", size: 50 * MiB, lfs: true, present: false },
  ],
  quants: [
    { name: "Q4_K_M", files: ["m.Q4_K_M.gguf"], size: 400 * MiB, present: false },
    {
      name: "F16",
      files: ["m.F16-00001-of-00002.gguf", "m.F16-00002-of-00002.gguf"],
      size: 900 * MiB,
      present: false,
    },
  ],
  configs: [
    {
      config: "a",
      default: true,
      splits: [
        { split: "train", status: "ready", num_bytes_parquet: 10 },
        { split: "test", status: "ready", num_bytes_parquet: 5 },
      ],
    },
    {
      config: "b",
      default: false,
      splits: [{ split: "train", status: "ready", num_bytes_parquet: 100 }],
    },
  ],
};

describe("Pull selections", () => {
  it("adds projectors and non-GGUF files to a quant selection", () => {
    expect(isProjector("mmproj-m-f16.gguf")).toBe(true);
    expect(isProjector("m.Q4_K_M.gguf")).toBe(false);
    expect(filesForQuants(detail, ["Q4_K_M"]).sort()).toEqual([
      "README.md",
      "m.Q4_K_M.gguf",
      "mmproj-m-f16.gguf",
    ]);
  });

  it("counts only bytes not already in the Arsenal", () => {
    expect(selectionBytes(detail, { quants: ["Q4_K_M"] })).toBe(1000 + 450 * MiB);
    expect(selectionBytes(detail, { quants: ["F16"] })).toBe(1000 + 650 * MiB);
    expect(selectionBytes(detail, {})).toBe(1000 + 1050 * MiB);
  });

  it("uses Parquet sizes for Parquet-only dataset Pulls", () => {
    expect(selectionBytes(detail, { parquet_only: true })).toBe(115);
    expect(selectionBytes(detail, { parquet_only: true, configs: ["a"], splits: ["train"] })).toBe(
      10,
    );
  });

  it("formats Discover filters for models and datasets", () => {
    expect(
      toFilterParams("models", { format: "gguf", task: undefined, library: "transformers" }),
    ).toEqual(["gguf", "transformers"]);
    expect(toFilterParams("datasets", { modality: "text", size_categories: "1K<n<10K" })).toEqual([
      "modality:text",
      "size_categories:1K<n<10K",
    ]);
  });
});
