import { File as NodeFile } from "node:buffer";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { afterEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { forgetUpload, rememberedUpload, rememberUpload, resumeMaxAge } from "./resume";
import { sha256Stream } from "./sha256";
import { partRange, pool, stage, uploadAndCommit, uploadObject } from "./upload";

function streamOf(bytes: Uint8Array, chunk: number): ReadableStream<Uint8Array> {
  let i = 0;
  return new ReadableStream({
    pull(c) {
      if (i >= bytes.length) {
        c.close();
        return;
      }
      c.enqueue(bytes.slice(i, i + chunk));
      i += chunk;
    },
  });
}

type Call = { method: string; url: string; body?: string; csrf: string | null };

function fakeFetch(handler: (req: Request) => Response | Promise<Response>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const req =
      input instanceof Request
        ? input
        : new Request(new URL(String(input), "http://hangar.test"), init);
    const clone = req.clone();
    const body = ["GET", "HEAD"].includes(req.method) ? undefined : await clone.text();
    calls.push({
      method: req.method,
      url: new URL(req.url).pathname + new URL(req.url).search,
      body,
      csrf: req.headers.get("X-CSRF-Token"),
    });
    return handler(req);
  });
  vi.stubGlobal("fetch", fn);
  return calls;
}

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(undefined);
  localStorage.clear();
});

describe("browser uploads", () => {
  it("hashes a stream in chunks exactly like a one-shot SHA-256", async () => {
    const bytes = new Uint8Array(3 * 1024 * 1024 + 17).map((_, i) => (i * 31) % 256);
    const seen: number[] = [];
    const hex = await sha256Stream(streamOf(bytes, 65536), (d) => seen.push(d));
    expect(hex).toBe(bytesToHex(sha256(bytes)));
    expect(seen.at(-1)).toBe(bytes.length);
    expect(await sha256Stream(streamOf(new Uint8Array(0), 1))).toBe(
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    );
  });

  it("stages files under a target folder and splits parts", () => {
    const f = new File(["x"], "a.bin");
    expect(stage([{ file: f, relativePath: "dir/a.bin" }], "/models/")).toEqual([
      { file: f, path: "models/dir/a.bin" },
    ]);
    expect(partRange(10, 4, 1)).toEqual([0, 4]);
    expect(partRange(10, 4, 3)).toEqual([8, 10]);
  });

  it("runs at most the limit at once", async () => {
    let running = 0;
    let peak = 0;
    const done: number[] = [];
    await pool([1, 2, 3, 4, 5, 6, 7, 8, 9], 4, async (n) => {
      running++;
      peak = Math.max(peak, running);
      await new Promise((r) => setTimeout(r, 5));
      done.push(n);
      running--;
    });
    expect(peak).toBe(4);
    expect(done.sort()).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9]);
  });

  it("resumes by skipping parts the server already has, then completes", async () => {
    setCsrfToken("csrf-1");
    const calls = fakeFetch((req) => {
      const u = new URL(req.url);
      if (req.method === "GET")
        return json({
          id: "u1",
          sha256: "s",
          size: 10,
          part_size: 4,
          part_count: 3,
          parts: [{ number: 1, etag: '"e1"', size: 4 }],
          state: "uploading",
        });
      if (req.method === "PUT")
        return json(
          {
            number: Number(u.pathname.split("/").pop()),
            etag: `"p${u.pathname.split("/").pop()}"`,
          },
          200,
          { ETag: `"p${u.pathname.split("/").pop()}"` },
        );
      return json({ id: "u1", sha256: "s", size: 10, part_count: 3, parts: [], state: "verified" });
    });
    const progress: number[] = [];
    const repo = { type: "models" as const, ns: "ada", name: "m" };
    await uploadObject(
      repo,
      {
        path: "w.bin",
        action: "upload",
        upload_id: "u1",
        part_size: 4,
        presigned: false,
        part_urls: [
          "/api/hangar/v1/repos/models/ada/m/uploads/u1/parts/1",
          "/api/hangar/v1/repos/models/ada/m/uploads/u1/parts/2",
          "/api/hangar/v1/repos/models/ada/m/uploads/u1/parts/3",
        ],
      },
      new NodeFile(["0123456789"], "w.bin") as unknown as File,
      "s",
      (n) => progress.push(n),
    );
    const puts = calls.filter((c) => c.method === "PUT");
    expect(puts.map((c) => c.url)).toEqual([
      "/api/hangar/v1/repos/models/ada/m/uploads/u1/parts/2",
      "/api/hangar/v1/repos/models/ada/m/uploads/u1/parts/3",
    ]);
    expect(puts.every((c) => c.csrf === "csrf-1")).toBe(true);
    const complete = calls.find((c) => c.url.endsWith("/complete"));
    expect(JSON.parse(complete?.body ?? "{}")).toEqual({
      parts: [
        { number: 1, etag: '"e1"' },
        { number: 2, etag: '"p2"' },
        { number: 3, etag: '"p3"' },
      ],
    });
    expect(progress[0]).toBe(4);
    expect(progress.at(-1)).toBe(10);
    expect(rememberedUpload({ type: "models", ns: "ada", name: "m" }, "s")).toBeUndefined();
  });

  // W2's plan starts a new multipart upload every time it is asked, so after
  // a reload the planned upload is empty. These tests model that server.
  const repo = { type: "models" as const, ns: "ada", name: "m" };
  const sha = "f".repeat(64);
  const partsOf = (id: string) =>
    [1, 2, 3].map((n) => `/api/hangar/v1/repos/models/ada/m/uploads/${id}/parts/${n}`);
  const freshPlan = (id: string, presigned = false) => ({
    path: "w.bin",
    action: "upload" as const,
    upload_id: id,
    part_size: 4,
    presigned,
    part_urls: presigned
      ? [1, 2, 3].map((n) => `https://s3.example.com/${id}/${n}?sig=x`)
      : partsOf(id),
  });
  type Upload = { parts: { number: number; etag: string; size: number }[]; completed?: boolean };
  function server(uploads: Record<string, Upload>, failPart?: { id: string; n: number }) {
    return fakeFetch((req) => {
      const path = new URL(req.url).pathname;
      if (new URL(req.url).host === "s3.example.com")
        return new Response(null, { status: 200, headers: { ETag: '"s3"' } });
      if (path.startsWith("/api/hangar/v1/repos/models/ada/m/uploads/")) {
        const [id, rest, n] = path.split("/uploads/")[1]?.split("/") ?? [];
        const u = uploads[id ?? ""];
        if (!u)
          return json({ status: 404, detail: "The upload does not exist or has expired." }, 404);
        if (req.method === "GET")
          return json({
            id,
            sha256: sha,
            size: 10,
            part_size: 4,
            part_count: 3,
            parts: u.parts,
            state: "uploading",
          });
        if (rest === "parts") {
          if (failPart && failPart.id === id && failPart.n === Number(n))
            return json({ status: 500, detail: "The Blob store failed." }, 500);
          const size = Number(n) === 3 ? 2 : 4;
          u.parts.push({ number: Number(n), etag: `"${id}-${n}"`, size });
          return json({ number: Number(n), etag: `"${id}-${n}"`, size }, 200, {
            ETag: `"${id}-${n}"`,
          });
        }
        u.completed = true;
        return json({
          id,
          sha256: sha,
          size: 10,
          part_count: 3,
          parts: u.parts,
          state: "verified",
        });
      }
      return new Response(null, { status: 599 });
    });
  }
  const file = () => new NodeFile(["0123456789"], "w.bin") as unknown as File;

  it("remembers the upload before sending parts, and resumes it after a reload", async () => {
    setCsrfToken("csrf-3");
    const uploads: Record<string, Upload> = { u1: { parts: [] } };
    let calls = server(uploads, { id: "u1", n: 2 });
    await expect(uploadObject(repo, freshPlan("u1"), file(), sha, () => {})).rejects.toThrow();
    await new Promise((r) => setTimeout(r, 10));
    expect(rememberedUpload(repo, sha)?.id).toBe("u1");
    const kept = uploads.u1?.parts.map((p) => p.number) ?? [];
    expect(kept).toContain(1);

    // The reload: a new plan gives a new, empty upload with presigned URLs.
    uploads.u2 = { parts: [] };
    calls = server(uploads);
    const progress: number[] = [];
    await uploadObject(repo, freshPlan("u2", true), file(), sha, (n) => progress.push(n));
    const puts = calls.filter((c) => c.method === "PUT").map((c) => c.url);
    const missing = [1, 2, 3].filter((n) => !kept.includes(n));
    expect(puts).toEqual(missing.map((n) => partsOf("u1")[n - 1]));
    expect(calls.some((c) => c.url.includes("/uploads/u2"))).toBe(false);
    expect(uploads.u1?.completed).toBe(true);
    const complete = calls.find((c) => c.url.endsWith("/u1/complete"));
    expect(
      JSON.parse(complete?.body ?? "{}").parts.map((p: { number: number }) => p.number),
    ).toEqual([1, 2, 3]);
    expect(progress[0]).toBe(kept.reduce((a, n) => a + (n === 3 ? 2 : 4), 0));
    expect(progress.at(-1)).toBe(10);
    expect(rememberedUpload(repo, sha)).toBeUndefined();
  });

  it("uses the planned upload when the remembered one has expired", async () => {
    rememberUpload(repo, sha, "gone", 10);
    const uploads: Record<string, Upload> = { u3: { parts: [] } };
    const calls = server(uploads);
    await uploadObject(repo, freshPlan("u3"), file(), sha, () => {});
    expect(calls.filter((c) => c.method === "PUT").map((c) => c.url)).toEqual(partsOf("u3"));
    expect(uploads.u3?.completed).toBe(true);
    expect(rememberedUpload(repo, sha)).toBeUndefined();
  });

  it("uses the planned upload when the remembered one has no parts or another size", async () => {
    const uploads: Record<string, Upload> = { empty: { parts: [] }, u4: { parts: [] } };
    rememberUpload(repo, sha, "empty", 10);
    let calls = server(uploads);
    await uploadObject(repo, freshPlan("u4", true), file(), sha, () => {});
    expect(calls.filter((c) => c.method === "PUT").map((c) => c.url)).toEqual(
      [1, 2, 3].map((n) => `/u4/${n}?sig=x`),
    );
    rememberUpload(repo, sha, "empty", 99);
    uploads.u5 = { parts: [] };
    calls = server(uploads);
    await uploadObject(repo, freshPlan("u5"), file(), sha, () => {});
    expect(calls.some((c) => c.url.includes("/uploads/empty"))).toBe(false);
    expect(uploads.u5?.completed).toBe(true);
  });

  it("keeps remembered uploads per Repo and object, drops old ones, and survives blocked storage", () => {
    const other = { ...repo, name: "other" };
    rememberUpload(repo, sha, "a", 1, 1000);
    rememberUpload(other, sha, "b", 1, 1000);
    expect(rememberedUpload(repo, sha, 1000)?.id).toBe("a");
    expect(rememberedUpload(other, sha, 1000)?.id).toBe("b");
    expect(rememberedUpload(repo, sha, 1000 + resumeMaxAge + 1)).toBeUndefined();
    rememberUpload(repo, "e".repeat(64), "c", 1, 1000 + resumeMaxAge + 1);
    expect(localStorage.getItem(`hangar-upload:models/ada/other/${sha}`)).toBeNull();
    forgetUpload(repo, "e".repeat(64));
    expect(localStorage.length).toBe(0);
    const blocked = {
      length: 0,
      key: () => null,
      getItem: () => {
        throw new DOMException("blocked", "SecurityError");
      },
      setItem: () => {
        throw new DOMException("blocked", "SecurityError");
      },
      removeItem: () => {
        throw new DOMException("blocked", "SecurityError");
      },
    };
    expect(() => rememberUpload(repo, sha, "x", 1, 0, blocked)).not.toThrow();
    expect(rememberedUpload(repo, sha, 0, blocked)).toBeUndefined();
    expect(() => forgetUpload(repo, sha, blocked)).not.toThrow();
  });

  it("sends nothing but the bytes to presigned URLs", async () => {
    const calls = fakeFetch((req) => {
      if (req.url.startsWith("https://s3.example.com"))
        return new Response(null, { status: 200, headers: { ETag: '"s3"' } });
      if (req.method === "GET") return json({}, 404);
      return json({ id: "u2", sha256: "s", size: 3, part_count: 1, parts: [], state: "verified" });
    });
    setCsrfToken("csrf-2");
    await uploadObject(
      { type: "models", ns: "a", name: "b" },
      {
        path: "x",
        action: "upload",
        upload_id: "u2",
        part_size: 3,
        presigned: true,
        part_urls: ["https://s3.example.com/part1?sig=abc"],
      },
      new NodeFile(["abc"], "x") as unknown as File,
      "s",
      () => {},
    );
    const put = calls.find((c) => c.method === "PUT");
    expect(put?.csrf).toBeNull();
  });

  it("commits inline, existing and uploaded files in one Commit", async () => {
    vi.stubGlobal("Worker", undefined);
    const calls = fakeFetch((req) => {
      const path = new URL(req.url).pathname;
      if (path.endsWith("/uploads"))
        return json({
          files: [
            { path: "README.md", action: "inline" },
            { path: "w.safetensors", action: "exists" },
          ],
        });
      return json({ sha: "c1", ref: "refs/heads/main" }, 201);
    });
    const res = await uploadAndCommit(
      { type: "models", ns: "ada", name: "m" },
      [
        { file: new NodeFile(["# hi"], "README.md") as unknown as File, path: "README.md" },
        {
          file: new NodeFile(["weights"], "w.safetensors") as unknown as File,
          path: "w.safetensors",
        },
      ],
      { summary: "Upload 2 files", branch: "main" },
      () => {},
    );
    expect(res.sha).toBe("c1");
    const plan = JSON.parse(calls[0]?.body ?? "{}");
    expect(plan.files[1]).toEqual({
      path: "w.safetensors",
      size: 7,
      sha256: bytesToHex(sha256(new TextEncoder().encode("weights"))),
    });
    const commit = JSON.parse(calls[1]?.body ?? "{}");
    expect(commit.operations).toEqual([
      { op: "add", path: "README.md", content_base64: btoa("# hi") },
      { op: "add_lfs", path: "w.safetensors", sha256: plan.files[1].sha256, size: 7 },
    ]);
  });
});
