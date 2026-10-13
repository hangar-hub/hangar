/** The kind of a Repo, as the UI and the API's singular type use it. */
export type RepoKind = "model" | "dataset";

/** The plural type the native API uses in paths. */
export function apiType(kind: RepoKind): "models" | "datasets" {
  return kind === "dataset" ? "datasets" : "models";
}

/** Returns the Repo ID as shown in the UI, with the datasets/ prefix for datasets. */
export function repoLabel(kind: RepoKind, ns: string, name: string): string {
  return `${kind === "dataset" ? "datasets/" : ""}${ns}/${name}`;
}

/** The UI path of a Repo page, matching the Hub's URLs. */
export function repoPath(kind: RepoKind, ns: string, name: string): string {
  return `/${kind === "dataset" ? "datasets/" : ""}${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;
}

/**
 * Encodes a Revision for a URL path segment the way the Hub does, so
 * `refs/pr/1` becomes `refs%2Fpr%2F1`.
 */
export function encodeRev(rev: string): string {
  return encodeURIComponent(rev);
}

/** Encodes a file path for a URL, keeping the slashes between segments. */
export function encodePath(path: string): string {
  return path
    .split("/")
    .map((s) => encodeURIComponent(s))
    .join("/");
}

/** The download URL of a file at a Revision (the Hub's `resolve` URL). */
export function resolveUrl(
  kind: RepoKind,
  ns: string,
  name: string,
  rev: string,
  path: string,
  download = false,
): string {
  const base = `${repoPath(kind, ns, name)}/resolve/${encodeRev(rev)}/${encodePath(path)}`;
  return download ? `${base}?download=true` : base;
}

/** The UI path of a folder at a Revision. */
export function treePath(kind: RepoKind, ns: string, name: string, rev: string, path = ""): string {
  const p = path ? `/${encodePath(path)}` : "";
  return `${repoPath(kind, ns, name)}/tree/${encodeRev(rev)}${p}`;
}

/** The UI path of a file at a Revision. */
export function blobPath(
  kind: RepoKind,
  ns: string,
  name: string,
  rev: string,
  path: string,
): string {
  return `${repoPath(kind, ns, name)}/blob/${encodeRev(rev)}/${encodePath(path)}`;
}

/** Splits a Repo ID such as `ns/name` or `datasets/ns/name`. */
export function parseRepoId(
  id: string,
  kind?: RepoKind,
): { kind: RepoKind; ns: string; name: string } | null {
  let rest = id.trim();
  let k: RepoKind = kind ?? "model";
  if (rest.startsWith("datasets/")) {
    k = "dataset";
    rest = rest.slice("datasets/".length);
  } else if (rest.startsWith("models/")) {
    k = "model";
    rest = rest.slice("models/".length);
  }
  const parts = rest.split("/");
  if (parts.length !== 2 || !parts[0] || !parts[1]) return null;
  return { kind: k, ns: parts[0], name: parts[1] };
}

/** The last segment of a path. */
export function baseName(path: string): string {
  const i = path.lastIndexOf("/");
  return i === -1 ? path : path.slice(i + 1);
}

/** The folder part of a path, or "" at the root. */
export function dirName(path: string): string {
  const i = path.lastIndexOf("/");
  return i === -1 ? "" : path.slice(0, i);
}

/** Joins path segments, dropping empty ones and stray slashes. */
export function joinPath(...parts: string[]): string {
  return parts
    .flatMap((p) => p.split("/"))
    .filter((s) => s !== "" && s !== ".")
    .join("/");
}

const textExtensions = new Set([
  "md",
  "txt",
  "json",
  "jsonl",
  "yaml",
  "yml",
  "toml",
  "py",
  "js",
  "ts",
  "tsx",
  "jsx",
  "sh",
  "cfg",
  "ini",
  "csv",
  "tsv",
  "jinja",
  "j2",
  "html",
  "css",
  "xml",
  "rs",
  "go",
  "c",
  "h",
  "cpp",
  "java",
  "gitattributes",
  "gitignore",
  "model",
  "tiktoken",
]);

/** The viewer to use for a file, from its name. */
export type FileView = "markdown" | "json" | "text" | "image" | "safetensors" | "gguf" | "binary";

export function fileView(path: string): FileView {
  const name = baseName(path).toLowerCase();
  const ext = name.includes(".") ? (name.split(".").pop() ?? "") : name.replace(/^\./, "");
  if (ext === "md" || ext === "markdown") return "markdown";
  if (ext === "json") return "json";
  if (ext === "safetensors") return "safetensors";
  if (ext === "gguf") return "gguf";
  if (["png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "bmp"].includes(ext)) return "image";
  if (textExtensions.has(ext) || name === "license" || name === "dockerfile") return "text";
  return "binary";
}

/** The Shiki language for a file, or "text". */
export function languageOf(path: string): string {
  const name = baseName(path).toLowerCase();
  const ext = name.split(".").pop() ?? "";
  const map: Record<string, string> = {
    py: "python",
    js: "javascript",
    ts: "typescript",
    tsx: "tsx",
    jsx: "jsx",
    json: "json",
    jsonl: "json",
    yaml: "yaml",
    yml: "yaml",
    toml: "toml",
    sh: "shellscript",
    md: "markdown",
    html: "html",
    css: "css",
    xml: "xml",
    rs: "rust",
    go: "go",
    c: "c",
    h: "c",
    cpp: "cpp",
    java: "java",
    jinja: "jinja",
    j2: "jinja",
    ini: "ini",
    cfg: "ini",
  };
  return map[ext] ?? "text";
}
