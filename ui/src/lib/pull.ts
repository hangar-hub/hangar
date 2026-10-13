import type { DiscoverDetail, Selection } from "@/api/types";

/** Projector files that vision GGUF models need with any quant (06 "GGUF quant picker"). */
export function isProjector(path: string): boolean {
  return /(^|\/)mmproj[^/]*\.gguf$/i.test(path);
}

/** Files a GGUF quant selection pulls: the chosen quants' files, projectors and every non-GGUF file. */
export function filesForQuants(
  detail: Pick<DiscoverDetail, "files" | "quants">,
  quants: string[],
): string[] {
  const chosen = new Set(
    (detail.quants ?? [])
      .filter((q) => quants.includes(q.name))
      .flatMap((q) => [...q.files, ...(q.projectors ?? [])]),
  );
  return detail.files
    .filter(
      (f) => !f.path.toLowerCase().endsWith(".gguf") || chosen.has(f.path) || isProjector(f.path),
    )
    .map((f) => f.path);
}

/**
 * Bytes a Pull would transfer for a selection, counting only files that are
 * not in the Arsenal yet. For Parquet-only dataset Pulls it uses the Splits'
 * Parquet sizes.
 */
export function selectionBytes(
  detail: Pick<DiscoverDetail, "files" | "quants" | "configs">,
  sel: Selection,
): number {
  if (sel.parquet_only && detail.configs) {
    return detail.configs
      .filter((c) => !sel.configs?.length || sel.configs.includes(c.config))
      .flatMap((c) => c.splits.filter((s) => !sel.splits?.length || sel.splits.includes(s.split)))
      .reduce((a, s) => a + (s.num_bytes_parquet ?? 0), 0);
  }
  let paths: Set<string> | null = null;
  if (sel.quants?.length) paths = new Set(filesForQuants(detail, sel.quants));
  return detail.files
    .filter((f) => !f.present && (!paths || paths.has(f.path)))
    .reduce((a, f) => a + f.size, 0);
}

/** The Hub filter tags for Discover's model and dataset facets. */
export const modelFilters = {
  format: ["gguf", "safetensors", "mlx", "onnx", "pytorch"],
  task: [
    "text-generation",
    "feature-extraction",
    "sentence-similarity",
    "fill-mask",
    "text-classification",
    "image-classification",
    "automatic-speech-recognition",
    "text-to-image",
  ],
  library: ["transformers", "sentence-transformers", "diffusers", "gguf", "mlx", "peft"],
};

export const datasetFilters = {
  modality: ["text", "image", "audio", "tabular", "video"],
  format: ["parquet", "csv", "json", "imagefolder", "audiofolder", "webdataset"],
  task_categories: [
    "text-classification",
    "question-answering",
    "text-generation",
    "image-classification",
    "automatic-speech-recognition",
  ],
  size_categories: ["n<1K", "1K<n<10K", "10K<n<100K", "100K<n<1M", "1M<n<10M"],
};

/** The `filter` values to send: plain tags for models, `facet:value` for datasets. */
export function toFilterParams(
  type: "models" | "datasets",
  selected: Record<string, string | undefined>,
): string[] {
  const out: string[] = [];
  for (const [facet, value] of Object.entries(selected)) {
    if (!value) continue;
    out.push(type === "datasets" ? `${facet}:${value}` : value);
  }
  return out;
}
