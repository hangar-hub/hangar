import { describe, expect, it } from "vitest";
import { ggufSummary, ggufValue, safetensorsTensors } from "./headers";

describe("header viewers", () => {
  it("totals safetensors parameters by dtype", () => {
    const s = safetensorsTensors({
      __metadata__: { format: "pt" },
      "layer.10.w": { dtype: "F16", shape: [4, 4], data_offsets: [0, 32] },
      "layer.2.w": { dtype: "F32", shape: [2, 3], data_offsets: [32, 56] },
      scalar: { dtype: "F32", shape: [], data_offsets: [56, 60] },
    });
    expect(s.tensors.map((t) => t.name)).toEqual(["layer.2.w", "layer.10.w", "scalar"]);
    expect(s.byDtype).toEqual({ F16: 16, F32: 7 });
    expect(s.total).toBe(23);
    expect(s.metadata).toEqual({ format: "pt" });
  });

  it("summarises GGUF metadata, tokenizer and tensors", () => {
    const g = ggufSummary({
      version: 3,
      metadata: {
        "general.architecture": "llama",
        "general.file_type": 15,
        "llama.context_length": 4096,
        "tokenizer.ggml.model": "llama",
        "tokenizer.ggml.tokens": { type: "array", length: 32000 },
        "tokenizer.ggml.bos_token_id": 1,
        "tokenizer.chat_template": "{{ messages }}",
      },
      tensors: [{ name: "a", shape: [2, 3], type: "Q4_K" }],
    });
    expect(g.architecture).toBe("llama");
    expect(g.contextLength).toBe(4096);
    expect(g.tokenizer).toEqual({ model: "llama", vocabSize: 32000, bos: 1, eos: undefined });
    expect(g.chatTemplate).toBe("{{ messages }}");
    expect(g.metadata.find(([k]) => k === "tokenizer.ggml.tokens")?.[1]).toBe("Array of 32000");
    expect(g.metadata.some(([k]) => k === "tokenizer.chat_template")).toBe(false);
    expect(g.total).toBe(6);
  });

  it("formats long arrays briefly", () => {
    expect(ggufValue([1, 2])).toBe("[1,2]");
    expect(ggufValue(Array.from({ length: 20 }, () => 0))).toBe("Array of 20");
    expect(ggufValue("x")).toBe("x");
  });
});
