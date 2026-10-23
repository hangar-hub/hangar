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
    expect(tf?.code).toContain(`os.environ["HF_ENDPOINT"] = "${origin}"`);
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
    // 08-local-tools.md: `hangar lmstudio install <ns>/<name>[:<quant>]`, no --quant flag.
    expect(s.find((x) => x.id === "lmstudio")?.code).toBe(
      `hangar lmstudio install --url ${origin} TinyLlama/Chat-GGUF:Q4_K_M\nhangar lmstudio install --url ${origin} TinyLlama/Chat-GGUF:Q8_0`,
    );
    expect(s.find((x) => x.id === "lmstudio")?.code).not.toContain("--quant");
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
    expect(st.find((x) => x.id === "lmstudio")?.code).toBe(
      `hangar lmstudio install --url ${origin} --revision v1 a/b`,
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

  it("gives only download snippets for a dataset of files the datasets library cannot read", () => {
    const s = snippets({
      kind: "dataset",
      id: "ada/sample-files",
      origin,
      readable: false,
      defaultBranch: "main",
    });
    expect(s.map((x) => x.id)).toEqual(["hf", "huggingface_hub", "git"]);
    expect(s[0]?.code).toBe(
      "export HF_ENDPOINT=https://hangar.example.com\nhf download ada/sample-files --repo-type dataset",
    );
    expect(s.some((x) => x.code.includes("load_dataset") || x.code.includes("parquet"))).toBe(
      false,
    );
  });

  it("points pasted Python at the Instance before any Hugging Face import", () => {
    const all = [
      ...snippets({
        kind: "model",
        id: "a/b",
        origin,
        library: "sentence-transformers",
        remoteCode: true,
      }),
      ...snippets({ kind: "model", id: "a/b", origin, library: "diffusers" }),
      ...snippets({ kind: "dataset", id: "ada/imdb", origin, defaultBranch: "main" }),
    ].filter((x) => x.language === "python");
    expect(all.map((x) => x.id).sort()).toEqual(
      [
        "datasets",
        "diffusers",
        "huggingface_hub",
        "huggingface_hub",
        "huggingface_hub",
        "pandas",
        "polars",
        "sentence-transformers",
        "transformers",
        "transformers",
      ].sort(),
    );
    for (const s of all) {
      // A comment is lost on paste: the endpoint must be in the code itself.
      expect(s.code, s.id).not.toMatch(/^#.*HF_ENDPOINT/m);
      const set = s.code.indexOf(`os.environ["HF_ENDPOINT"] = "${origin}"`);
      const endpointArg = s.code.includes(`endpoint="${origin}"`);
      expect(set >= 0 || endpointArg, s.id).toBe(true);
      if (set >= 0) {
        const firstImport = s.code.search(/^(from|import) (?!os$)/m);
        expect(set, s.id).toBeLessThan(firstImport);
      }
    }
    const hub = all.find((x) => x.id === "huggingface_hub");
    expect(hub?.code).toContain(`snapshot_download("a/b", endpoint="${origin}")`);
    // Polars' own hf:// client ignores HF_ENDPOINT, so it reads through
    // huggingface_hub with the Instance as the endpoint.
    const polars = all.find((x) => x.id === "polars");
    expect(polars?.code).toContain(`HfFileSystem(endpoint="${origin}")`);
    expect(polars?.code).not.toContain("hf://");
  });

  it("uses the Revision being viewed in every client", () => {
    const m = snippets({ kind: "model", id: "a/b", origin, revision: "refs/pr/3" });
    expect(m.find((x) => x.id === "transformers")?.code).toContain(
      'AutoModel.from_pretrained("a/b", revision="refs/pr/3")',
    );
    expect(m.find((x) => x.id === "huggingface_hub")?.code).toContain(
      `snapshot_download("a/b", revision="refs/pr/3", endpoint="${origin}")`,
    );
    expect(m.find((x) => x.id === "hf")?.code).toContain("hf download a/b --revision refs/pr/3");
    expect(m.find((x) => x.id === "vllm")?.code).toContain("vllm serve a/b --revision refs/pr/3");
    // git clone --branch takes branches and tags only.
    expect(m.find((x) => x.id === "git")?.code).toContain(
      `git clone ${origin}/a/b && cd b && git fetch origin refs/pr/3 && git checkout FETCH_HEAD`,
    );
    const sha = "0123456789abcdef0123456789abcdef01234567";
    const atSha = snippets({ kind: "model", id: "a/b", origin, revision: sha });
    expect(atSha.find((x) => x.id === "git")?.code).toContain(
      `GIT_LFS_SKIP_SMUDGE=1 git clone ${origin}/a/b && cd b && GIT_LFS_SKIP_SMUDGE=1 git checkout ${sha}`,
    );
    const tag = snippets({ kind: "model", id: "a/b", origin, revision: "v1.0" });
    expect(tag.find((x) => x.id === "git")?.code).toContain(
      `git clone --branch v1.0 ${origin}/a/b`,
    );
    const d = snippets({
      kind: "dataset",
      id: "ada/imdb",
      origin,
      revision: "refs/convert/parquet",
      dataFile: "default/train/0000.parquet",
    });
    expect(d.find((x) => x.id === "pandas")?.code).toContain(
      '"hf://datasets/ada/imdb@refs%2Fconvert%2Fparquet/default/train/0000.parquet"',
    );
    expect(d.find((x) => x.id === "duckdb")?.code).toContain(
      "/resolve/refs%2Fconvert%2Fparquet/default/train/0000.parquet",
    );
    // Shell metacharacters in a branch name are quoted.
    const odd = snippets({ kind: "model", id: "a/b", origin, revision: "fix;rm" });
    expect(odd.find((x) => x.id === "hf")?.code).toContain("--revision 'fix;rm'");
  });
});
