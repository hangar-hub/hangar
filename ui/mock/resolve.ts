import type { MockState } from "./data";
import { fileSize, findRepo, resolveRev } from "./repo-model";

/**
 * Answers the Hub's `resolve` URLs for files in mock Repos. Large LFS files
 * answer with a short stand-in body, so tests never move real model bytes.
 */
export function resolveFile(
  state: MockState,
  path: string,
): { status: number; headers: Record<string, string>; body?: string | Buffer } | null {
  const m = /^\/(datasets\/)?([^/]+)\/([^/]+)\/resolve\/([^/]+)\/(.+)$/.exec(path);
  if (!m) return null;
  const [, ds, ns, name, rev, file] = m as unknown as [
    string,
    string | undefined,
    string,
    string,
    string,
    string,
  ];
  const hubError = (status: number, code: string, message: string) => ({
    status,
    headers: {
      "Content-Type": "application/json",
      "X-Error-Code": code,
      "X-Error-Message": message,
    },
    body: JSON.stringify({ error: message }),
  });
  if (!state.signedIn) return hubError(401, "RepoNotFound", "Repository Not Found");
  const r = findRepo(
    state,
    ds ? "datasets" : "models",
    decodeURIComponent(ns),
    decodeURIComponent(name),
  );
  if (!r) return hubError(404, "RepoNotFound", "Repository Not Found");
  const c = resolveRev(r, decodeURIComponent(rev));
  if (!c) return hubError(404, "RevisionNotFound", "Revision Not Found");
  const filePath = file.split("/").map(decodeURIComponent).join("/");
  const f = c.files.find((x) => x.path === filePath);
  if (!f) return hubError(404, "EntryNotFound", "Entry not found");
  if (f.present === false)
    return hubError(404, "EntryNotFound", "This file is not in the arsenal yet.");
  const base = filePath.split("/").pop() ?? "file";
  const headers: Record<string, string> = {
    "X-Repo-Commit": c.sha,
    "Content-Disposition": `inline; filename="${base}"`,
    "Content-Type": base.endsWith(".svg")
      ? "image/svg+xml"
      : base.endsWith(".png")
        ? "image/png"
        : "text/plain; charset=utf-8",
  };
  if (f.text !== undefined) return { status: 200, headers, body: f.text };
  if (base.endsWith(".png")) {
    // A 1 by 1 transparent PNG.
    return {
      status: 200,
      headers,
      body: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
        "base64",
      ),
    };
  }
  headers["X-Linked-Size"] = String(fileSize(f));
  headers["Content-Type"] = "application/octet-stream";
  return { status: 200, headers, body: Buffer.alloc(64) };
}
