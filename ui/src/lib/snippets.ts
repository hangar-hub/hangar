/**
 * Code snippets for the **Use this model** panel (09-web-ui.md), filled in
 * for the current Repo, Revision and Instance URL.
 */

export type SnippetInput = {
  kind: "model" | "dataset";
  id: string; // namespace/name
  origin: string; // the Instance URL, such as https://hangar.example.com
  /** The Revision being viewed: omitted for the default branch. */
  revision?: string;
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

/** Quotes a word for a POSIX shell when it holds anything but safe characters. */
function sh(s: string): string {
  return /^[\w@%+=:,./-]+$/.test(s) ? s : `'${s.replaceAll("'", `'"'"'`)}'`;
}

/**
 * The lines that point a Python library at this Instance. Libraries built
 * on huggingface_hub read HF_ENDPOINT once, when huggingface_hub is first
 * imported, so the code sets it before any import. A commented `export`
 * line did nothing once pasted, and the library went to huggingface.co.
 */
function pythonEndpoint(origin: string): string {
  return `import os\n\n# Set this before any Hugging Face import: they read it once, on import.\nos.environ["HF_ENDPOINT"] = ${q(origin)}\n\n`;
}

/** A Revision in an hf:// path, where a slash in it is percent-encoded. */
function hfRev(revision?: string): string {
  return revision ? `@${encodeURIComponent(revision)}` : "";
}

export function snippets(i: SnippetInput): Snippet[] {
  const env = `HF_ENDPOINT=${i.origin}`;
  const py = pythonEndpoint(i.origin);
  const revArg = i.revision ? `, revision=${q(i.revision)}` : "";
  const out: Snippet[] = [];
  if (i.kind === "dataset" && i.readable === false) {
    return [cliSnippet(i, env), hubSnippet(i, revArg), gitSnippet(i)];
  }
  if (i.kind === "dataset") {
    const cfg = i.config && i.config !== "default" ? `, ${q(i.config)}` : "";
    const split = i.split ? `, split=${q(i.split)}` : "";
    out.push({
      id: "datasets",
      label: "datasets",
      language: "python",
      code: `${py}from datasets import load_dataset\n\nds = load_dataset(${q(i.id)}${cfg}${split}${revArg})`,
    });
    const file = i.dataFile ?? "**/*.parquet";
    const rev = i.revision ?? i.defaultBranch ?? "main";
    const filesPath = `datasets/${i.id}${hfRev(i.revision)}/${file}`;
    out.push({
      id: "pandas",
      label: "pandas",
      language: "python",
      code: `${py}import pandas as pd\n\ndf = pd.read_parquet(${q(`hf://${filesPath}`)})`,
    });
    // Polars has its own hf:// client, which always calls huggingface.co,
    // so it reads the files through huggingface_hub (docs/guides/datasets.md).
    out.push({
      id: "polars",
      label: "Polars",
      language: "python",
      code: `import polars as pl\nfrom huggingface_hub import HfFileSystem\n\nfs = HfFileSystem(endpoint=${q(i.origin)})\npaths = fs.glob(${q(filesPath)})\ndf = pl.concat([pl.read_parquet(fs.open(p, "rb")) for p in paths])`,
    });
    out.push({
      id: "duckdb",
      label: "DuckDB",
      language: "sql",
      code: `SELECT * FROM read_parquet('${i.origin}/datasets/${i.id}/resolve/${encodeURIComponent(rev)}/${i.dataFile ?? "data/train.parquet"}') LIMIT 10;`,
    });
    out.push(hubSnippet(i, revArg));
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
      code: `${py}from transformers import AutoModel, AutoTokenizer\n\ntokenizer = AutoTokenizer.from_pretrained(${q(i.id)}${revArg}${trust})\nmodel = AutoModel.from_pretrained(${q(i.id)}${revArg}${trust})`,
    });
  }
  out.push(hubSnippet(i, revArg));
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
  const lmFlags = `--url ${i.origin}${i.revision ? ` --revision ${sh(i.revision)}` : ""}`;
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
      code: `${env} vllm serve ${i.id}${i.revision ? ` --revision ${sh(i.revision)}` : ""}${i.remoteCode ? " --trust-remote-code" : ""}`,
    });
  }
  if (i.library === "sentence-transformers") {
    out.push({
      id: "sentence-transformers",
      label: "sentence-transformers",
      language: "python",
      code: `${py}from sentence_transformers import SentenceTransformer\n\nmodel = SentenceTransformer(${q(i.id)}${revArg}${trust})`,
    });
  }
  if (i.library === "diffusers") {
    out.push({
      id: "diffusers",
      label: "diffusers",
      language: "python",
      code: `${py}from diffusers import DiffusionPipeline\n\npipe = DiffusionPipeline.from_pretrained(${q(i.id)}${revArg})`,
    });
  }
  return out;
}

/**
 * snapshot_download takes the Instance as its endpoint argument, which
 * works even when huggingface_hub was already imported, as in a notebook.
 */
function hubSnippet(i: SnippetInput, revArg: string): Snippet {
  const type = i.kind === "dataset" ? `, repo_type="dataset"` : "";
  return {
    id: "huggingface_hub",
    label: "huggingface_hub",
    language: "python",
    code: `from huggingface_hub import snapshot_download\n\npath = snapshot_download(${q(i.id)}${type}${revArg}, endpoint=${q(i.origin)})`,
  };
}

function cliSnippet(i: SnippetInput, env: string): Snippet {
  const type = i.kind === "dataset" ? " --repo-type dataset" : "";
  const rev = i.revision ? ` --revision ${sh(i.revision)}` : "";
  return {
    id: "hf",
    label: "hf CLI",
    language: "shellscript",
    code: `export ${env}\nhf download ${i.id}${type}${rev}`,
  };
}

/**
 * `clone --branch` takes a branch or a tag. A Commit SHA, or a ref such as
 * refs/pr/1, is checked out after the clone instead.
 */
function gitSnippet(i: SnippetInput): Snippet {
  const url = `${i.origin}/${i.kind === "dataset" ? "datasets/" : ""}${i.id}`;
  const rev = i.revision;
  const named = Boolean(rev) && !/^[0-9a-f]{40}$/.test(rev ?? "") && !rev?.startsWith("refs/");
  const branch = rev && named ? ` --branch ${sh(rev)}` : "";
  // `env` goes before each git command that can download LFS files.
  const clone = (env: string) => {
    let line = `${env}git clone${branch} ${url}`;
    if (rev && !named) {
      const dir = sh(i.id.split("/").pop() ?? i.id);
      line += rev.startsWith("refs/")
        ? ` && cd ${dir} && git fetch origin ${sh(rev)} && ${env}git checkout FETCH_HEAD`
        : ` && cd ${dir} && ${env}git checkout ${rev}`;
    }
    return line;
  };
  return {
    id: "git",
    label: "git",
    language: "shellscript",
    code: `${clone("")}\n\n# Without downloading large files:\n${clone("GIT_LFS_SKIP_SMUDGE=1 ")}`,
  };
}
