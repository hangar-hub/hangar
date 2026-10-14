import { api, apiBase, rawFetch } from "@/api/client";
import type { CommitOperation, UploadedPart, UploadPlanItem, UploadStatus } from "@/api/types";
import { ApiError, unwrap } from "@/lib/problem";
import { joinPath } from "@/lib/repo";
import { forgetUpload, type RepoKey, rememberedUpload, rememberUpload } from "./resume";

/** A file staged for upload, with its path in the Repo. */
export type StagedFile = { file: File; path: string };

export type FileProgress = {
  path: string;
  size: number;
  hashed: number;
  uploaded: number;
  state: "waiting" | "hashing" | "uploading" | "stored" | "inline" | "done" | "error";
  error?: string;
};

/** How many part uploads run at once (09-web-ui.md: default 4). */
export const partConcurrency = 4;

/** Staged paths for files from a picker or drop, under a target folder. */
export function stage(files: { file: File; relativePath: string }[], folder: string): StagedFile[] {
  return files.map(({ file, relativePath }) => ({
    file,
    path: joinPath(folder, relativePath || file.name),
  }));
}

/** Reads every file under dropped items, following folders (DataTransfer entries). */
export async function filesFromDrop(
  items: DataTransferItemList,
): Promise<{ file: File; relativePath: string }[]> {
  const out: { file: File; relativePath: string }[] = [];
  const walk = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
    if (entry.isFile) {
      const file = await new Promise<File>((res, rej) =>
        (entry as FileSystemFileEntry).file(res, rej),
      );
      out.push({ file, relativePath: joinPath(prefix, file.name) });
    } else if (entry.isDirectory) {
      const reader = (entry as FileSystemDirectoryEntry).createReader();
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((res, rej) =>
          reader.readEntries(res, rej),
        );
        if (batch.length === 0) break;
        for (const child of batch) await walk(child, joinPath(prefix, entry.name));
      }
    }
  };
  const entries = [...items]
    .map((i) => i.webkitGetAsEntry?.())
    .filter((e): e is FileSystemEntry => Boolean(e));
  if (entries.length === 0) {
    for (const i of [...items]) {
      const f = i.getAsFile();
      if (f) out.push({ file: f, relativePath: f.name });
    }
    return out;
  }
  for (const e of entries) await walk(e, "");
  return out;
}

/** Hashes a file in a Web Worker, falling back to the main thread where workers are unavailable. */
export function hashFile(file: File, onProgress: (done: number) => void): Promise<string> {
  if (typeof Worker === "undefined") {
    return import("./sha256").then((m) => m.sha256Stream(file.stream(), onProgress));
  }
  return new Promise((resolve, reject) => {
    const w = new Worker(new URL("./hash.worker.ts", import.meta.url), { type: "module" });
    w.onmessage = (
      e: MessageEvent<{ type: string; done?: number; hex?: string; message?: string }>,
    ) => {
      if (e.data.type === "progress") onProgress(e.data.done ?? 0);
      if (e.data.type === "done") {
        w.terminate();
        resolve(e.data.hex ?? "");
      }
      if (e.data.type === "error") {
        w.terminate();
        reject(new Error(e.data.message));
      }
    };
    w.onerror = (e) => {
      w.terminate();
      reject(new Error(e.message || "The file could not be hashed."));
    };
    w.postMessage({ file });
  });
}

/** The byte range of a part, numbered from 1. */
export function partRange(size: number, partSize: number, n: number): [number, number] {
  const start = (n - 1) * partSize;
  return [start, Math.min(size, start + partSize)];
}

/** Runs tasks with at most `limit` at once, failing on the first error. */
export async function pool<T>(
  items: T[],
  limit: number,
  fn: (item: T) => Promise<void>,
  signal?: AbortSignal,
): Promise<void> {
  let next = 0;
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (next < items.length) {
      if (signal?.aborted) throw new DOMException("The upload was cancelled.", "AbortError");
      const item = items[next++] as T;
      await fn(item);
    }
  });
  await Promise.all(workers);
}

async function putPart(
  url: string,
  body: Blob,
  presigned: boolean,
  signal?: AbortSignal,
): Promise<string> {
  // Presigned URLs go straight to the Blob store with no Hangar cookies
  // or headers; Hangar part URLs carry the session and CSRF token.
  const res = presigned
    ? await fetch(url, { method: "PUT", body, signal, credentials: "omit" })
    : await rawFetch(url, {
        method: "PUT",
        body,
        signal,
        headers: { "Content-Type": "application/octet-stream" },
      });
  if (!res.ok) {
    const problem = await res.json().catch(() => undefined);
    throw new ApiError(res.status, problem, `A part upload failed with status ${res.status}.`);
  }
  const header = res.headers.get("ETag");
  if (header) return header;
  const json = (await res.json().catch(() => ({}))) as { etag?: string };
  if (json.etag) return json.etag;
  throw new Error(
    "The Blob store did not return an ETag. Ask an admin to expose the ETag header in the bucket's CORS rules.",
  );
}

type RepoPath = RepoKey;

/** Hangar's own part URLs for an upload, used to resume an earlier upload. */
export function hangarPartUrls(repo: RepoPath, id: string, count: number): string[] {
  const base = `${apiBase}/repos/${repo.type}/${encodeURIComponent(repo.ns)}/${encodeURIComponent(repo.name)}/uploads/${encodeURIComponent(id)}/parts/`;
  return Array.from({ length: Math.max(1, count) }, (_, i) => `${base}${i + 1}`);
}

/** The parts an upload holds, or undefined when it is gone or cannot be read. */
async function uploadStatus(repo: RepoPath, id: string): Promise<UploadStatus | undefined> {
  try {
    const res = await api.GET("/repos/{type}/{ns}/{name}/uploads/{id}", {
      params: { path: { ...repo, id } },
    });
    return res.response.ok ? res.data : undefined;
  } catch {
    return undefined;
  }
}

type Target = {
  id: string;
  urls: string[];
  partSize: number;
  presigned: boolean;
  done: Map<number, UploadedPart>;
};

const doneParts = (s: UploadStatus | undefined) =>
  new Map((s?.parts ?? []).map((p): [number, UploadedPart] => [p.number, p]));

/**
 * Picks the upload to send an object's parts to. An earlier upload of the
 * same object in this browser (remembered across reloads) wins when the
 * server still holds some of its parts: its missing parts go to Hangar's
 * part URLs, because the plan only gives URLs for the upload it just
 * started. Otherwise the planned upload is used, resuming it when the
 * server already holds parts of it.
 */
async function pickTarget(
  repo: RepoPath,
  item: UploadPlanItem,
  file: File,
  sha256: string,
): Promise<Target | "verified"> {
  const earlier = rememberedUpload(repo, sha256);
  if (earlier && earlier.id !== item.upload_id && earlier.size === file.size) {
    const s = await uploadStatus(repo, earlier.id);
    if (s && s.sha256 === sha256 && s.size === file.size) {
      if (s.state === "verified") return "verified";
      if (s.part_size && s.part_size > 0 && s.parts.length > 0) {
        return {
          id: earlier.id,
          urls: hangarPartUrls(repo, earlier.id, s.part_count),
          partSize: s.part_size,
          presigned: false,
          done: doneParts(s),
        };
      }
    }
    forgetUpload(repo, sha256);
  }
  const id = item.upload_id;
  const urls = item.part_urls ?? [];
  if (!id || urls.length === 0)
    throw new Error(`The Instance gave no upload URLs for ${item.path}.`);
  const s = await uploadStatus(repo, id);
  if (s?.state === "verified") return "verified";
  return {
    id,
    urls,
    partSize: item.part_size ?? file.size,
    presigned: Boolean(item.presigned),
    done: doneParts(s),
  };
}

/**
 * Uploads one planned LFS object: asks which parts the server already
 * holds (so a reload resumes), uploads the rest in parallel, then
 * completes the upload so the server verifies the SHA-256.
 */
export async function uploadObject(
  repo: RepoPath,
  item: UploadPlanItem,
  file: File,
  sha256: string,
  onBytes: (uploaded: number) => void,
  signal?: AbortSignal,
): Promise<void> {
  const target = await pickTarget(repo, item, file, sha256);
  if (target === "verified") {
    forgetUpload(repo, sha256);
    onBytes(file.size);
    return;
  }
  const { id, urls, partSize, presigned, done } = target;
  rememberUpload(repo, sha256, id, file.size);
  let uploaded = [...done.values()].reduce((a, p) => {
    const [start, end] = partRange(file.size, partSize, p.number);
    return a + (p.size ?? end - start);
  }, 0);
  onBytes(uploaded);
  const todo = urls.map((url, i) => ({ url, n: i + 1 })).filter((p) => !done.has(p.n));
  await pool(
    todo,
    partConcurrency,
    async ({ url, n }) => {
      const [start, end] = partRange(file.size, partSize, n);
      const etag = await putPart(url, file.slice(start, end), presigned, signal);
      done.set(n, { number: n, etag, size: end - start });
      uploaded += end - start;
      onBytes(uploaded);
    },
    signal,
  );
  const parts = [...done.values()]
    .sort((a, b) => a.number - b.number)
    .map((p) => ({ number: p.number, etag: p.etag }));
  try {
    unwrap(
      await api.POST("/repos/{type}/{ns}/{name}/uploads/{id}/complete", {
        params: { path: { ...repo, id } },
        body: { parts },
      }),
    );
  } catch (e) {
    // The server refused this upload's parts, so the next try starts afresh.
    if (e instanceof ApiError && e.status < 500) forgetUpload(repo, sha256);
    throw e;
  }
  forgetUpload(repo, sha256);
}

/** Base64 of a file's bytes, for small files sent inline in the Commit. */
export async function base64Of(file: Blob): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(bin);
}

export type UploadOptions = {
  summary: string;
  description?: string;
  branch?: string;
  createPr?: boolean;
};

/**
 * Uploads staged files and commits them (UP-3): hash each file, plan the
 * upload, send small text files inline, skip objects the Instance already
 * has, upload the rest in parts, then make one Commit.
 */
export async function uploadAndCommit(
  repo: RepoPath,
  files: StagedFile[],
  opts: UploadOptions,
  onProgress: (p: FileProgress[]) => void,
  signal?: AbortSignal,
) {
  const progress: FileProgress[] = files.map((f) => ({
    path: f.path,
    size: f.file.size,
    hashed: 0,
    uploaded: 0,
    state: "waiting",
  }));
  const emit = () => onProgress(progress.map((p) => ({ ...p })));
  const shas: string[] = [];
  for (let i = 0; i < files.length; i++) {
    const p = progress[i] as FileProgress;
    p.state = "hashing";
    emit();
    shas[i] = await hashFile((files[i] as StagedFile).file, (done) => {
      p.hashed = done;
      emit();
    });
    if (signal?.aborted) throw new DOMException("The upload was cancelled.", "AbortError");
  }
  const plan = unwrap(
    await api.POST("/repos/{type}/{ns}/{name}/uploads", {
      params: { path: repo },
      body: {
        revision: opts.branch,
        create_pr: opts.createPr,
        files: files.map((f, i) => ({
          path: f.path,
          size: f.file.size,
          sha256: shas[i] as string,
        })),
      },
    }),
  );
  const ops: CommitOperation[] = [];
  for (let i = 0; i < files.length; i++) {
    const f = files[i] as StagedFile;
    const p = progress[i] as FileProgress;
    const item = plan.files.find((x) => x.path === f.path);
    if (!item) throw new Error(`The Instance did not plan ${f.path}.`);
    if (item.action === "inline") {
      ops.push({ op: "add", path: f.path, content_base64: await base64Of(f.file) });
      p.state = "inline";
      p.uploaded = f.file.size;
    } else if (item.action === "exists") {
      forgetUpload(repo, shas[i] as string);
      ops.push({ op: "add_lfs", path: f.path, sha256: shas[i], size: f.file.size });
      p.state = "stored";
      p.uploaded = f.file.size;
    } else {
      p.state = "uploading";
      emit();
      await uploadObject(
        repo,
        item,
        f.file,
        shas[i] as string,
        (n) => {
          p.uploaded = n;
          emit();
        },
        signal,
      );
      ops.push({ op: "add_lfs", path: f.path, sha256: shas[i], size: f.file.size });
      p.state = "done";
    }
    emit();
  }
  return unwrap(
    await api.POST("/repos/{type}/{ns}/{name}/commits", {
      params: { path: repo },
      body: {
        summary: opts.summary,
        description: opts.description,
        branch: opts.branch,
        create_pr: opts.createPr,
        operations: ops,
      },
    }),
  );
}
