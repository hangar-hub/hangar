import { File as NodeFile } from "node:buffer";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { afterEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
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
