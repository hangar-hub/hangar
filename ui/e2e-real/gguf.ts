// Writes small GGUF files (version 3) for the Discover and GGUF viewer flows:
// a llama header with a tokenizer, a chat template and a few F32 tensors of
// zeros. llama.cpp's file format: https://github.com/ggml-org/ggml/blob/master/docs/gguf.md

const alignment = 32;

/** GGUF metadata value types. */
const type = { u32: 4, string: 8, array: 9 } as const;

/** ggml tensor type F32. */
const f32 = 0;

/** llama.cpp `general.file_type` values of the quants the flows use. */
export const fileTypes = { Q4_K_M: 15, Q8_0: 7 } as const;

export const chatTemplate =
  "{% for message in messages %}<|{{ message.role }}|>\n{{ message.content }}</s>\n{% endfor %}";

/** The tokenizer's vocabulary: three special tokens and a few pieces. */
export const vocabulary = ["<unk>", "<s>", "</s>", "▁hello", "▁world", "▁arsenal"];

/** The tensors, as ggml lists dimensions: innermost first. */
export const tensors: { name: string; dims: number[] }[] = [
  { name: "token_embd.weight", dims: [8, vocabulary.length] },
  { name: "blk.0.attn_q.weight", dims: [8, 8] },
  { name: "blk.0.attn_k.weight", dims: [8, 8] },
  { name: "output.weight", dims: [8, vocabulary.length] },
];

class Writer {
  private parts: Buffer[] = [];
  length = 0;
  private push(b: Buffer) {
    this.parts.push(b);
    this.length += b.length;
  }
  u32(n: number) {
    const b = Buffer.alloc(4);
    b.writeUInt32LE(n);
    this.push(b);
  }
  u64(n: number) {
    const b = Buffer.alloc(8);
    b.writeBigUInt64LE(BigInt(n));
    this.push(b);
  }
  str(s: string) {
    const b = Buffer.from(s, "utf8");
    this.u64(b.length);
    this.push(b);
  }
  bytes(b: Buffer) {
    this.push(b);
  }
  pad() {
    const rest = this.length % alignment;
    if (rest) this.push(Buffer.alloc(alignment - rest));
  }
  done(): Buffer {
    return Buffer.concat(this.parts);
  }
}

/** A GGUF file whose header says it holds the given quant. */
export function gguf(name: string, quant: keyof typeof fileTypes): Buffer {
  const w = new Writer();
  const str =
    (v: string) =>
    (w: Writer): void => {
      w.u32(type.string);
      w.str(v);
    };
  const u32 =
    (v: number) =>
    (w: Writer): void => {
      w.u32(type.u32);
      w.u32(v);
    };
  const kv: [string, (w: Writer) => void][] = [
    ["general.architecture", str("llama")],
    ["general.name", str(name)],
    ["general.file_type", u32(fileTypes[quant])],
    ["llama.context_length", u32(2048)],
    ["llama.embedding_length", u32(8)],
    ["llama.block_count", u32(1)],
    ["tokenizer.ggml.model", str("llama")],
    [
      "tokenizer.ggml.tokens",
      (w) => {
        w.u32(type.array);
        w.u32(type.string);
        w.u64(vocabulary.length);
        for (const t of vocabulary) w.str(t);
      },
    ],
    ["tokenizer.ggml.bos_token_id", u32(1)],
    ["tokenizer.ggml.eos_token_id", u32(2)],
    ["tokenizer.chat_template", str(chatTemplate)],
  ];
  w.bytes(Buffer.from("GGUF", "ascii"));
  w.u32(3);
  w.u64(tensors.length);
  w.u64(kv.length);
  for (const [key, write] of kv) {
    w.str(key);
    write(w);
  }
  let offset = 0;
  const sizes: number[] = [];
  for (const t of tensors) {
    w.str(t.name);
    w.u32(t.dims.length);
    for (const d of t.dims) w.u64(d);
    w.u32(f32);
    w.u64(offset);
    const size = t.dims.reduce((a, b) => a * b, 1) * 4;
    sizes.push(size);
    offset += Math.ceil(size / alignment) * alignment;
  }
  w.pad();
  for (const size of sizes) {
    w.bytes(Buffer.alloc(size));
    w.pad();
  }
  return w.done();
}
