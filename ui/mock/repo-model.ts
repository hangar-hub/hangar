import type { components } from "../src/api/schema";
import {
  type MockCommit,
  type MockFile,
  type MockRepo,
  type MockState,
  sha1,
  sha256,
} from "./data.ts";

type S = components["schemas"];

/** Finds a Repo by plural type and ID, case-insensitively as Hangar does. */
export function findRepo(
  state: MockState,
  type: string,
  ns: string,
  name: string,
): MockRepo | undefined {
  const t = type === "datasets" || type === "dataset" ? "dataset" : "model";
  return state.repos.find(
    (r) =>
      r.repo.type === t &&
      r.repo.namespace.toLowerCase() === ns.toLowerCase() &&
      r.repo.name.toLowerCase() === name.toLowerCase() &&
      !r.repo.deleted_at,
  );
}

export function allCommits(r: MockRepo): MockCommit[] {
  return r.commits;
}

/** Resolves a branch, tag, PR ref or (short) SHA to a Commit. */
export function resolveRev(r: MockRepo, rev: string | undefined): MockCommit | undefined {
  const name = rev || r.repo.default_branch;
  const target =
    r.branches[name] ??
    r.branches[name.replace(/^refs\/heads\//, "")] ??
    r.tags[name.replace(/^refs\/tags\//, "")] ??
    prRef(r, name);
  if (target)
    return r.commits.find((c) => c.sha === target) ?? r.prCommits?.find((c) => c.sha === target);
  if (/^[0-9a-f]{7,40}$/.test(name)) {
    return [...r.commits, ...(r.prCommits ?? [])].find((c) => c.sha.startsWith(name));
  }
  return undefined;
}

function prRef(r: MockRepo, name: string): string | undefined {
  const m = /^refs\/pr\/(\d+)$/.exec(name);
  if (!m) return undefined;
  const d = r.discussions.find((x) => x.num === Number(m[1]) && x.kind === "pull_request");
  return d ? (r.prHeads?.[d.num] ?? r.commits[0]?.sha) : undefined;
}

export function fileSize(f: MockFile): number {
  return f.size ?? Buffer.byteLength(f.text ?? "");
}

export function fileOid(f: MockFile): string {
  return sha1(`blob:${f.path}:${f.text ?? f.size}`);
}

export function lfsOid(f: MockFile): string {
  return sha256(`lfs:${f.path}:${f.size}`);
}

/** The last Commit that changed a path, walking back from a Commit. */
export function lastCommitFor(r: MockRepo, from: MockCommit, path: string): MockCommit {
  const chain: MockCommit[] = [];
  let c: MockCommit | undefined = from;
  const all = [...r.commits, ...(r.prCommits ?? [])];
  while (c) {
    chain.push(c);
    const parent: string | undefined = c.parents[0];
    c = parent ? all.find((x) => x.sha === parent) : undefined;
  }
  let last = from;
  for (let i = 0; i < chain.length; i++) {
    const cur = chain[i] as MockCommit;
    const prev = chain[i + 1];
    const a = cur.files.filter((f) => f.path === path || f.path.startsWith(`${path}/`));
    const b = prev
      ? prev.files.filter((f) => f.path === path || f.path.startsWith(`${path}/`))
      : [];
    const sig = (fs: MockFile[]) =>
      fs
        .map((f) => `${f.path}:${f.text ?? ""}:${f.size ?? ""}`)
        .sort()
        .join("|");
    if (sig(a) !== sig(b)) {
      last = cur;
      break;
    }
  }
  return last;
}

export function treeEntry(r: MockRepo, c: MockCommit, f: MockFile): S["TreeEntry"] {
  const last = lastCommitFor(r, c, f.path);
  const e: S["TreeEntry"] = {
    path: f.path,
    type: "file",
    size: fileSize(f),
    oid: fileOid(f),
    present: f.present !== false,
    last_commit: { sha: last.sha, title: last.title, date: last.date },
  };
  if (f.lfs) e.lfs = { oid: lfsOid(f), size: fileSize(f), pointer_size: 132 };
  if (f.scan) e.scan_status = f.scan;
  return e;
}

/** Lists a folder (or the whole tree when recursive), folders first. */
export function listTree(
  r: MockRepo,
  c: MockCommit,
  dir: string,
  recursive: boolean,
): S["TreeEntry"][] | null {
  const prefix = dir ? `${dir}/` : "";
  const inside = c.files.filter((f) => f.path.startsWith(prefix));
  if (dir && inside.length === 0) return null;
  if (recursive)
    return inside.map((f) => treeEntry(r, c, f)).sort((a, b) => a.path.localeCompare(b.path));
  const dirs = new Map<string, MockFile[]>();
  const files: MockFile[] = [];
  for (const f of inside) {
    const rest = f.path.slice(prefix.length);
    const slash = rest.indexOf("/");
    if (slash === -1) files.push(f);
    else {
      const d = prefix + rest.slice(0, slash);
      dirs.set(d, [...(dirs.get(d) ?? []), f]);
    }
  }
  const dirEntries: S["TreeEntry"][] = [...dirs.entries()].map(([d, fs]) => {
    const last = lastCommitFor(r, c, d);
    return {
      path: d,
      type: "directory",
      size: fs.reduce((a, f) => a + fileSize(f), 0),
      oid: sha1(`tree:${d}`),
      present: fs.every((f) => f.present !== false),
      last_commit: { sha: last.sha, title: last.title, date: last.date },
    };
  });
  return [
    ...dirEntries.sort((a, b) => a.path.localeCompare(b.path)),
    ...files.map((f) => treeEntry(r, c, f)).sort((a, b) => a.path.localeCompare(b.path)),
  ];
}

/** File-level changes between a Commit and its first parent. */
export function changesOf(r: MockRepo, c: MockCommit): S["FileChange"][] {
  const all = [...r.commits, ...(r.prCommits ?? [])];
  const parent = c.parents[0] ? all.find((x) => x.sha === c.parents[0]) : undefined;
  const before = new Map((parent?.files ?? []).map((f) => [f.path, f]));
  const after = new Map(c.files.map((f) => [f.path, f]));
  const out: S["FileChange"][] = [];
  for (const [path, f] of after) {
    const old = before.get(path);
    const binary = f.text === undefined;
    if (!old) {
      out.push({
        path,
        status: "added",
        new_oid: fileOid(f),
        new_size: fileSize(f),
        lfs: Boolean(f.lfs),
        binary,
      });
    } else if ((old.text ?? old.size) !== (f.text ?? f.size)) {
      out.push({
        path,
        status: "modified",
        old_oid: fileOid(old),
        new_oid: fileOid(f),
        old_size: fileSize(old),
        new_size: fileSize(f),
        lfs: Boolean(f.lfs),
        binary,
      });
    }
  }
  for (const [path, f] of before) {
    if (!after.has(path)) {
      out.push({
        path,
        status: "deleted",
        old_oid: fileOid(f),
        old_size: fileSize(f),
        lfs: Boolean(f.lfs),
        binary: f.text === undefined,
      });
    }
  }
  return out.sort((a, b) => a.path.localeCompare(b.path));
}

/** A unified diff of two texts, from a longest common subsequence. */
export function unifiedDiff(path: string, a: string, b: string): string {
  const x = a.split("\n");
  const y = b.split("\n");
  const n = x.length;
  const m = y.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      (lcs[i] as number[])[j] =
        x[i] === y[j]
          ? ((lcs[i + 1] as number[])[j + 1] as number) + 1
          : Math.max((lcs[i + 1] as number[])[j] as number, (lcs[i] as number[])[j + 1] as number);
    }
  }
  const lines: string[] = [`--- a/${path}`, `+++ b/${path}`, `@@ -1,${n} +1,${m} @@`];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      lines.push(` ${x[i]}`);
      i++;
      j++;
    } else if (((lcs[i + 1] as number[])[j] as number) >= ((lcs[i] as number[])[j + 1] as number)) {
      lines.push(`-${x[i]}`);
      i++;
    } else {
      lines.push(`+${y[j]}`);
      j++;
    }
  }
  while (i < n) lines.push(`-${x[i++]}`);
  while (j < m) lines.push(`+${y[j++]}`);
  return lines.join("\n");
}

const quantPattern = /(IQ\d_[A-Z]+|Q\d_K_[SML]|Q\d_K|Q\d_\d|BF16|F16|F32|MXFP4)/i;

/** Groups GGUF files by the quant in their names (06 "GGUF quant picker"). */
export function quantGroups(
  files: { path: string; size: number; present: boolean }[],
): S["QuantGroup"][] {
  const groups = new Map<string, S["QuantGroup"]>();
  for (const f of files) {
    if (!f.path.toLowerCase().endsWith(".gguf") || f.path.includes("mmproj")) continue;
    const q = (quantPattern.exec(f.path)?.[1] ?? "unknown").toUpperCase();
    const g = groups.get(q) ?? { name: q, files: [], size: 0, present: true };
    g.files.push(f.path);
    g.size += f.size;
    g.present = g.present && f.present;
    groups.set(q, g);
  }
  return [...groups.values()].sort((a, b) => a.size - b.size);
}
