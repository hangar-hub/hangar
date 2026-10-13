/**
 * Reads safetensors and GGUF headers from `GET .../files/{rev}/{path}/header`
 * into rows for the header viewers (BROWSE-4).
 *
 * safetensors: `header` is the file's JSON header as stored (tensor name to
 * `{dtype, shape, data_offsets}`, plus `__metadata__`). GGUF: `header` has
 * `version`, `tensor_count`, `metadata` (key to value; long arrays may be
 * summarised as `{type: "array", length}`) and `tensors` (`{name, shape,
 * type}`). docs/build/requests/w8.md records this shape for W7.
 */

export type TensorRow = { name: string; dtype: string; shape: number[]; params: number };

export function elementCount(shape: number[]): number {
  return shape.reduce((a, b) => a * b, 1);
}

export function safetensorsTensors(header: Record<string, unknown>): {
  tensors: TensorRow[];
  metadata: Record<string, string>;
  byDtype: Record<string, number>;
  total: number;
} {
  const tensors: TensorRow[] = [];
  let metadata: Record<string, string> = {};
  for (const [name, v] of Object.entries(header)) {
    if (name === "__metadata__") {
      metadata = (v ?? {}) as Record<string, string>;
      continue;
    }
    const t = v as { dtype?: string; shape?: number[] };
    const shape = Array.isArray(t?.shape) ? t.shape : [];
    tensors.push({ name, dtype: t?.dtype ?? "unknown", shape, params: elementCount(shape) });
  }
  tensors.sort((a, b) => a.name.localeCompare(b.name, "en", { numeric: true }));
  const byDtype: Record<string, number> = {};
  for (const t of tensors) byDtype[t.dtype] = (byDtype[t.dtype] ?? 0) + t.params;
  return { tensors, metadata, byDtype, total: tensors.reduce((a, t) => a + t.params, 0) };
}

/** How a GGUF metadata value reads in a table cell. */
export function ggufValue(v: unknown): string {
  if (Array.isArray(v)) return v.length > 8 ? `Array of ${v.length}` : JSON.stringify(v);
  if (v && typeof v === "object") {
    const o = v as { type?: string; length?: number };
    if (o.type === "array" && typeof o.length === "number") return `Array of ${o.length}`;
    return JSON.stringify(v);
  }
  return String(v);
}

function arrayLength(v: unknown): number | undefined {
  if (Array.isArray(v)) return v.length;
  if (v && typeof v === "object" && typeof (v as { length?: unknown }).length === "number")
    return (v as { length: number }).length;
  return undefined;
}

export type GgufSummary = {
  version?: number;
  architecture?: string;
  name?: string;
  fileType?: number;
  contextLength?: number;
  tokenizer: { model?: string; vocabSize?: number; bos?: number; eos?: number };
  chatTemplate?: string;
  metadata: [string, string][];
  tensors: TensorRow[];
  total: number;
};

/** llama.cpp `general.file_type` values, for the quant name. */
export const ggufFileTypes: Record<number, string> = {
  0: "F32",
  1: "F16",
  2: "Q4_0",
  3: "Q4_1",
  7: "Q8_0",
  8: "Q5_0",
  9: "Q5_1",
  10: "Q2_K",
  11: "Q3_K_S",
  12: "Q3_K_M",
  13: "Q3_K_L",
  14: "Q4_K_S",
  15: "Q4_K_M",
  16: "Q5_K_S",
  17: "Q5_K_M",
  18: "Q6_K",
  19: "IQ2_XXS",
  20: "IQ2_XS",
  23: "IQ3_XXS",
  24: "IQ1_S",
  25: "IQ4_NL",
  26: "IQ3_S",
  27: "IQ3_M",
  28: "IQ2_S",
  29: "IQ2_M",
  30: "IQ4_XS",
  31: "IQ1_M",
  32: "BF16",
};

export function ggufSummary(header: Record<string, unknown>): GgufSummary {
  const meta = (header.metadata ?? {}) as Record<string, unknown>;
  const arch =
    typeof meta["general.architecture"] === "string"
      ? (meta["general.architecture"] as string)
      : undefined;
  const num = (k: string) => (typeof meta[k] === "number" ? (meta[k] as number) : undefined);
  const rawTensors = Array.isArray(header.tensors)
    ? (header.tensors as { name?: string; shape?: number[]; type?: string }[])
    : [];
  const tensors = rawTensors.map((t) => {
    const shape = Array.isArray(t.shape) ? t.shape : [];
    return { name: t.name ?? "", dtype: t.type ?? "unknown", shape, params: elementCount(shape) };
  });
  return {
    version: typeof header.version === "number" ? header.version : undefined,
    architecture: arch,
    name: typeof meta["general.name"] === "string" ? (meta["general.name"] as string) : undefined,
    fileType: num("general.file_type"),
    contextLength: arch ? num(`${arch}.context_length`) : undefined,
    tokenizer: {
      model:
        typeof meta["tokenizer.ggml.model"] === "string"
          ? (meta["tokenizer.ggml.model"] as string)
          : undefined,
      vocabSize: arrayLength(meta["tokenizer.ggml.tokens"]),
      bos: num("tokenizer.ggml.bos_token_id"),
      eos: num("tokenizer.ggml.eos_token_id"),
    },
    chatTemplate:
      typeof meta["tokenizer.chat_template"] === "string"
        ? (meta["tokenizer.chat_template"] as string)
        : undefined,
    metadata: Object.entries(meta)
      .filter(([k]) => k !== "tokenizer.chat_template")
      .map(([k, v]) => [k, ggufValue(v)] as [string, string])
      .sort((a, b) => a[0].localeCompare(b[0])),
    tensors,
    total: tensors.reduce((a, t) => a + t.params, 0),
  };
}
