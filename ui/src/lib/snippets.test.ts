import { describe, expect, it } from "vitest";
import { snippets } from "./snippets";

const origin = "https://hangar.example.com";

describe("Use this model snippets", () => {
  it("fills in transformers with trust_remote_code when the Repo has code Dependencies", () => {
    const s = snippets({
      kind: "model",
      id: "ml-team/coder",
      origin,
      remoteCode: true,
      library: "transformers",
      formats: ["safetensors"],
    });
    const tf = s.find((x) => x.id === "transformers");
    expect(tf?.code).toContain(`HF_ENDPOINT=${origin}`);
    expect(tf?.code).toContain(
      'AutoModel.from_pretrained("ml-team/coder", trust_remote_code=True)',
    );
    expect(s.find((x) => x.id === "vllm")?.code).toBe(
      `HF_ENDPOINT=${origin} vllm serve ml-team/coder --trust-remote-code`,
    );
    expect(s.find((x) => x.id === "git")?.code).toContain(
      "GIT_LFS_SKIP_SMUDGE=1 git clone https://hangar.example.com/ml-team/coder",
    );
    expect(s.some((x) => x.id === "ollama")).toBe(false);
  });

  it("lists an Ollama pull per quant for GGUF Repos and skips transformers", () => {
    const s = snippets({
      kind: "model",
      id: "TinyLlama/Chat-GGUF",
      origin,
      formats: ["gguf"],
      quants: ["Q4_K_M", "Q8_0"],
    });
    expect(s.find((x) => x.id === "ollama")?.code).toBe(
      "ollama pull hangar.example.com/TinyLlama/Chat-GGUF:Q4_K_M\nollama pull hangar.example.com/TinyLlama/Chat-GGUF:Q8_0",
    );
    expect(s.find((x) => x.id === "lmstudio")?.code).toBe(
      "hangar lmstudio install TinyLlama/Chat-GGUF --quant Q4_K_M",
    );
    expect(s.some((x) => x.id === "transformers")).toBe(false);
  });

  it("adds library-specific snippets and Revisions", () => {
    const st = snippets({
      kind: "model",
      id: "a/b",
      origin,
      library: "sentence-transformers",
      revision: "v1",
    });
    expect(st.find((x) => x.id === "sentence-transformers")?.code).toContain(
      'SentenceTransformer("a/b", revision="v1")',
    );
    expect(st.find((x) => x.id === "hf")?.code).toBe(
      `export HF_ENDPOINT=${origin}\nhf download a/b --revision v1`,
    );
    expect(
      snippets({ kind: "model", id: "a/b", origin, library: "diffusers" }).some(
        (x) => x.id === "diffusers",
      ),
    ).toBe(true);
  });

  it("gives datasets, pandas, Polars and DuckDB for dataset Repos", () => {
    const s = snippets({
      kind: "dataset",
      id: "ada/imdb",
      origin,
      config: "plain",
      split: "train",
      dataFile: "data/train.parquet",
      defaultBranch: "main",
    });
    expect(s.map((x) => x.id)).toEqual([
      "datasets",
      "pandas",
      "polars",
      "duckdb",
      "huggingface_hub",
      "hf",
      "git",
    ]);
    expect(s[0]?.code).toContain('load_dataset("ada/imdb", "plain", split="train")');
    expect(s[1]?.code).toContain('pd.read_parquet("hf://datasets/ada/imdb/data/train.parquet")');
    expect(s[3]?.code).toBe(
      "SELECT * FROM read_parquet('https://hangar.example.com/datasets/ada/imdb/resolve/main/data/train.parquet') LIMIT 10;",
    );
    expect(s.find((x) => x.id === "huggingface_hub")?.code).toContain('repo_type="dataset"');
    expect(s.find((x) => x.id === "git")?.code).toContain(
      "https://hangar.example.com/datasets/ada/imdb",
    );
  });
});
