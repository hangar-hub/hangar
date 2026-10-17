/**
 * Code snippets for the **Use this model** panel (09-web-ui.md), filled in
 * for the current Repo, Revision and Instance URL.
 */

export type SnippetInput = {
  kind: "model" | "dataset";
  id: string; // namespace/name
  origin: string; // the Instance URL, such as https://hangar.example.com
  revision?: string; // omitted for the default branch
  library?: string;
  formats?: string[];
  quants?: string[];
  /** The Repo has code Dependencies, so transformers needs trust_remote_code. */
  remoteCode?: boolean;
  config?: string;
  split?: string;
  /** A Parquet file path for DuckDB, such as data/train-00000.parquet. */
  dataFile?: string;
  /**
   * False for a dataset with no data files the datasets library reads,
   * such as one that holds sample files for another project: only the
   * download snippets apply to it.
   */
  readable?: boolean;
  defaultBranch?: string;
};

export type Snippet = { id: string; label: string; language: string; code: string };

function hostOf(origin: string): string {
  try {
    return new URL(origin).host;
  } catch {
    return origin.replace(/^https?:\/\//, "");
  }
}

function q(s: string): string {
  return JSON.stringify(s);
}

export function snippets(i: SnippetInput): Snippet[] {
  const env = `HF_ENDPOINT=${i.origin}`;
  const revArg = i.revision ? `, revision=${q(i.revision)}` : "";
  const out: Snippet[] = [];
  if (i.kind === "dataset" && i.readable === false) {
    return [cliSnippet(i, env), hubSnippet(i, env, revArg), gitSnippet(i)];
  }
  if (i.kind === "dataset") {
    const cfg = i.config && i.config !== "default" ? `, ${q(i.config)}` : "";
    const split = i.split ? `, split=${q(i.split)}` : "";
    out.push({
      id: "datasets",
      label: "datasets",
      language: "python",
      code: `# export ${env}\nfrom datasets import load_dataset\n\nds = load_dataset(${q(i.id)}${cfg}${split}${revArg})`,
    });
    const file = i.dataFile ?? "**/*.parquet";
    const rev = i.revision ?? i.defaultBranch ?? "main";
    const hfPath = `hf://datasets/${i.id}${i.revision ? `@${i.revision}` : ""}/${file}`;
    out.push({
      id: "pandas",
      label: "pandas",
      language: "python",
      code: `# export ${env}\nimport pandas as pd\n\ndf = pd.read_parquet(${q(hfPath)})`,
    });
    out.push({
      id: "polars",
      label: "Polars",
      language: "python",
      code: `# export ${env}\nimport polars as pl\n\ndf = pl.scan_parquet(${q(hfPath)}).collect()`,
    });
    out.push({
      id: "duckdb",
      label: "DuckDB",
      language: "sql",
      code: `SELECT * FROM read_parquet('${i.origin}/datasets/${i.id}/resolve/${encodeURIComponent(rev)}/${i.dataFile ?? "data/train.parquet"}') LIMIT 10;`,
    });
    out.push(hubSnippet(i, env, revArg));
    out.push(cliSnippet(i, env));
    out.push(gitSnippet(i));
    return out;
  }
  const trust = i.remoteCode ? ", trust_remote_code=True" : "";
  const isGguf = i.formats?.includes("gguf") || i.library === "gguf";
  if (!isGguf) {
    out.push({
      id: "transformers",
      label: "Python (transformers)",
      language: "python",
      code: `# export ${env}\nfrom transformers import AutoModel, AutoTokenizer\n\ntokenizer = AutoTokenizer.from_pretrained(${q(i.id)}${revArg}${trust})\nmodel = AutoModel.from_pretrained(${q(i.id)}${revArg}${trust})`,
    });
  }
  out.push(hubSnippet(i, env, revArg));
  out.push(cliSnippet(i, env));
  out.push(gitSnippet(i));
  if (isGguf && i.quants?.length) {
    const host = hostOf(i.origin);
    out.push({
      id: "ollama",
      label: "Ollama",
      language: "shellscript",
      code: i.quants.map((quant) => `ollama pull ${host}/${i.id}:${quant}`).join("\n"),
    });
  }
  // 08-local-tools.md: `hangar lmstudio install <ns>/<name>[:<quant>]`,
  // one line per quant for GGUF Repos, as for Ollama.
  const lmFlags = `--url ${i.origin}${i.revision ? ` --revision ${i.revision}` : ""}`;
  const lmRefs = isGguf && i.quants?.length ? i.quants.map((quant) => `${i.id}:${quant}`) : [i.id];
  out.push({
    id: "lmstudio",
    label: "LM Studio",
    language: "shellscript",
    code: lmRefs.map((ref) => `hangar lmstudio install ${lmFlags} ${ref}`).join("\n"),
  });
  if (!isGguf) {
    out.push({
      id: "vllm",
      label: "vLLM",
      language: "shellscript",
      code: `${env} vllm serve ${i.id}${i.revision ? ` --revision ${i.revision}` : ""}${i.remoteCode ? " --trust-remote-code" : ""}`,
    });
  }
  if (i.library === "sentence-transformers") {
    out.push({
      id: "sentence-transformers",
      label: "sentence-transformers",
      language: "python",
      code: `# export ${env}\nfrom sentence_transformers import SentenceTransformer\n\nmodel = SentenceTransformer(${q(i.id)}${revArg}${trust})`,
    });
  }
  if (i.library === "diffusers") {
    out.push({
      id: "diffusers",
      label: "diffusers",
      language: "python",
      code: `# export ${env}\nfrom diffusers import DiffusionPipeline\n\npipe = DiffusionPipeline.from_pretrained(${q(i.id)}${revArg})`,
    });
  }
  return out;
}

function hubSnippet(i: SnippetInput, env: string, revArg: string): Snippet {
  const type = i.kind === "dataset" ? `, repo_type="dataset"` : "";
  return {
    id: "huggingface_hub",
    label: "huggingface_hub",
    language: "python",
    code: `# export ${env}\nfrom huggingface_hub import snapshot_download\n\npath = snapshot_download(${q(i.id)}${type}${revArg})`,
  };
}

function cliSnippet(i: SnippetInput, env: string): Snippet {
  const type = i.kind === "dataset" ? " --repo-type dataset" : "";
  const rev = i.revision ? ` --revision ${i.revision}` : "";
  return {
    id: "hf",
    label: "hf CLI",
    language: "shellscript",
    code: `export ${env}\nhf download ${i.id}${type}${rev}`,
  };
}

function gitSnippet(i: SnippetInput): Snippet {
  const url = `${i.origin}/${i.kind === "dataset" ? "datasets/" : ""}${i.id}`;
  const branch = i.revision ? ` --branch ${i.revision}` : "";
  return {
    id: "git",
    label: "git",
    language: "shellscript",
    code: `git clone${branch} ${url}\n\n# Without downloading large files:\nGIT_LFS_SKIP_SMUDGE=1 git clone${branch} ${url}`,
  };
}
