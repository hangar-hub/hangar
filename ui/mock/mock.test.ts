// @vitest-environment node
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { handlers } from "./handlers.ts";
import { loadSpec } from "./openapi.ts";
import { createMock } from "./server.ts";

let server: Server;
let base = "";
let cookie = "";

async function call(
  method: string,
  path: string,
  body?: unknown,
  extra: Record<string, string> = {},
) {
  const headers: Record<string, string> = { cookie, ...extra };
  if (body !== undefined && !(body instanceof Uint8Array))
    headers["Content-Type"] = "application/json";
  if (method !== "GET" && !headers["X-CSRF-Token"]) headers["X-CSRF-Token"] = "mock-csrf-token";
  const res = await fetch(`${base}${path}`, {
    method,
    headers,
    body:
      body === undefined
        ? undefined
        : body instanceof Uint8Array
          ? new Blob([body as Uint8Array<ArrayBuffer>])
          : JSON.stringify(body),
  });
  const set = res.headers.getSetCookie();
  for (const c of set) {
    const [pair] = c.split(";");
    if (pair?.startsWith("mock_session=")) cookie = pair;
  }
  const text = await res.text();
  let json: unknown;
  try {
    json = JSON.parse(text);
  } catch {
    json = text;
  }
  return {
    status: res.status,
    json: json as Record<string, unknown> & { items?: Record<string, unknown>[] },
    headers: res.headers,
  };
}

const api = "/api/hangar/v1";
const MiB = 1024 * 1024;

beforeAll(async () => {
  const mw = createMock();
  server = createServer((req, res) => {
    mw(req, res, () => {
      res.statusCode = 404;
      res.end("not mocked");
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

afterAll(() => {
  server.close();
});

describe("mock server", () => {
  it("has a handler for every operation in api/openapi.yaml and no others", () => {
    const ids = loadSpec()
      .routes.map((r) => r.operationId)
      .sort();
    expect(Object.keys(handlers).sort()).toEqual([...new Set(ids)].sort());
  });

  it("answers 401 until signed in and refuses unknown paths", async () => {
    // Nobody signed in is not an error for /me, as on Hangar.
    expect((await call("GET", `${api}/me`)).status).toBe(204);
    expect((await call("GET", `${api}/me/tokens`)).status).toBe(401);
    expect((await call("GET", `${api}/auth/providers`)).status).toBe(200);
    const wrong = await call("POST", `${api}/auth/login`, {
      username: "ada",
      password: "wrong-password",
    });
    expect(wrong.status).toBe(401);
    // W5's text (internal/api/auth.go), which the sign-in page shows.
    expect(wrong.json.detail).toBe("Wrong username or password.");
    expect((await call("POST", `${api}/auth/login`, { username: "ada" })).status).toBe(400);
    const me = await call("POST", `${api}/auth/login`, {
      username: "ada",
      password: "correct-horse",
    });
    expect(me.status).toBe(200);
    expect(me.json.username).toBe("ada");
    expect((await call("GET", `${api}/nope`)).status).toBe(404);
    expect(
      (
        await call(
          "POST",
          `${api}/me/tokens`,
          { name: "x", scope: "read" },
          { "X-CSRF-Token": "wrong" },
        )
      ).status,
    ).toBe(403);
  });

  it("answers every read with a response that matches the OpenAPI file", async () => {
    const reads = [
      "/me",
      "/me/sessions",
      "/me/tokens",
      "/me/ollama-keys",
      "/me/upstream-tokens/huggingface",
      "/search?q=ada",
      "/repos?sort=likes&format=gguf",
      "/repos/models/ada/bert-tiny-finetune",
      "/repos/models/ada/bert-tiny-finetune/refs",
      "/repos/models/ada/bert-tiny-finetune/commits?rev=main",
      "/repos/models/ada/bert-tiny-finetune/tree/main",
      "/repos/models/ada/bert-tiny-finetune/tree/main/onnx",
      "/repos/models/ada/bert-tiny-finetune/tree/v1.0/README.md",
      "/repos/models/ada/bert-tiny-finetune/metadata",
      "/repos/models/ada/bert-tiny-finetune/files/main/model.safetensors/header",
      "/repos/models/TinyLlama/TinyLlama-1.1B-Chat-GGUF/files/main/tinyllama-1.1b-chat.Q4_K_M.gguf/header",
      "/repos/models/TinyLlama/TinyLlama-1.1B-Chat-GGUF/quants",
      "/repos/models/TinyLlama/TinyLlama-1.1B-Chat-GGUF/mirror",
      "/repos/models/ada/bert-tiny-finetune/dependencies",
      "/repos/models/ada/bert-tiny-finetune/stats",
      "/repos/models/ada/bert-tiny-finetune/collaborators",
      "/repos/models/ada/legacy-checkpoint/access-requests",
      "/repos/datasets/ada/imdb-mini/configs",
      "/repos/models/ada/bert-tiny-finetune/discussions",
      "/repos/models/ada/bert-tiny-finetune/discussions/2",
      "/upstreams",
      "/discover/models?search=gguf",
      "/discover/models/TinyLlama/TinyLlama-1.1B-Chat-GGUF",
      "/discover/datasets/stanfordnlp/imdb",
      "/jobs?mine=true",
      "/namespaces/ml-team",
      "/orgs/ml-team/members",
      "/orgs/ml-team/members/ada",
      "/collections",
      "/collections/ada/small-and-fast",
      "/webhooks",
      "/admin/users",
      "/admin/upstreams",
      "/admin/settings",
      "/admin/storage",
      "/admin/audit",
      "/admin/system",
      "/admin/tokens",
    ];
    for (const path of reads) {
      const r = await call("GET", `${api}${path}`);
      expect(r.status, `${path}: ${JSON.stringify(r.json)}`).toBe(200);
    }
    const sha = (await call("GET", `${api}/repos/models/ada/bert-tiny-finetune/commits`)).json
      .items?.[0]?.sha as string;
    const detail = await call("GET", `${api}/repos/models/ada/bert-tiny-finetune/commits/${sha}`);
    expect(detail.status).toBe(200);
    const diff = await call(
      "GET",
      `${api}/repos/models/ada/bert-tiny-finetune/commits/${sha}/diff?path=README.md`,
    );
    expect(diff.status).toBe(200);
    expect(String(diff.json.patch)).toContain("+| Accuracy | 0.81 |");
    const csv = await call("GET", `${api}/admin/audit?format=csv`);
    expect(csv.headers.get("content-type")).toContain("text/csv");
  });

  it("uploads in parts and commits, then shows the file", async () => {
    const sha = "a".repeat(64);
    const plan = await call("POST", `${api}/repos/models/ada/bert-tiny-finetune/uploads`, {
      files: [
        { path: "notes.txt", size: 5, sha256: "b".repeat(64) },
        { path: "big.safetensors", size: 20 * MiB, sha256: sha },
      ],
    });
    expect(plan.status).toBe(200);
    const files = plan.json.files as {
      action: string;
      upload_id?: string;
      part_size?: number;
      part_urls?: string[];
    }[];
    expect(files.map((f) => f.action)).toEqual(["inline", "upload"]);
    const up = files[1] as { upload_id: string; part_urls: string[]; part_size: number };
    // W2's repo.PartSize: never under 16 MiB, so 20 MiB is two parts.
    expect(up.part_size).toBe(16 * MiB);
    expect(up.part_urls).toHaveLength(2);
    // Part URLs are absolute, as W2 builds them from server.public_url.
    expect(up.part_urls[0]).toBe(
      `${base}${api}/repos/models/ada/bert-tiny-finetune/uploads/${up.upload_id}/parts/1`,
    );
    const partPath = (u: string | undefined) => new URL(u ?? "").pathname;
    const p1 = await call("PUT", partPath(up.part_urls[0]), new Uint8Array(16 * MiB), {
      "Content-Type": "application/octet-stream",
    });
    expect(p1.status).toBe(200);
    const status = await call(
      "GET",
      `${api}/repos/models/ada/bert-tiny-finetune/uploads/${up.upload_id}`,
    );
    expect((status.json.parts as unknown[]).length).toBe(1);
    expect(
      (
        await call(
          "POST",
          `${api}/repos/models/ada/bert-tiny-finetune/uploads/${up.upload_id}/complete`,
          { parts: [{ number: 1, etag: "x" }] },
        )
      ).status,
    ).toBe(400);
    // Like W2, asking again continues the upload in progress.
    const again = await call("POST", `${api}/repos/models/ada/bert-tiny-finetune/uploads`, {
      files: [{ path: "big.safetensors", size: 20 * MiB, sha256: sha }],
    });
    const second = (again.json.files as { upload_id: string }[])[0];
    expect(second?.upload_id).toBe(up.upload_id);
    await call("PUT", partPath(up.part_urls[1]), new Uint8Array(4 * MiB), {
      "Content-Type": "application/octet-stream",
    });
    const done = await call(
      "POST",
      `${api}/repos/models/ada/bert-tiny-finetune/uploads/${up.upload_id}/complete`,
      {
        parts: [
          { number: 1, etag: "x" },
          { number: 2, etag: "y" },
        ],
      },
    );
    expect(done.json.state).toBe("verified");
    const commit = await call("POST", `${api}/repos/models/ada/bert-tiny-finetune/commits`, {
      summary: "Add weights",
      operations: [
        { op: "add", path: "notes.txt", content_base64: Buffer.from("hello").toString("base64") },
        { op: "add_lfs", path: "big.safetensors", sha256: sha, size: 20 * MiB },
      ],
    });
    expect(commit.status).toBe(201);
    const tree = await call("GET", `${api}/repos/models/ada/bert-tiny-finetune/tree/main`);
    expect(tree.json.items?.map((e) => e.path)).toEqual(
      expect.arrayContaining(["notes.txt", "big.safetensors"]),
    );
    const raw = await fetch(`${base}/ada/bert-tiny-finetune/resolve/main/notes.txt`, {
      headers: { cookie },
    });
    expect(await raw.text()).toBe("hello");
    const stale = await call("POST", `${api}/repos/models/ada/bert-tiny-finetune/commits`, {
      summary: "Stale",
      parent_commit: "0".repeat(40),
      operations: [{ op: "delete", path: "notes.txt" }],
    });
    expect(stale.status).toBe(412);
    // The head the editor started from is accepted; once someone else has
    // committed, the same parent is stale and refused, not overwritten.
    const repoPath = `${api}/repos/models/ada/bert-tiny-finetune`;
    const head = (await call("GET", repoPath)).json.sha as string;
    const edit = (text: string) => ({
      summary: "Edit notes",
      parent_commit: head,
      operations: [
        { op: "add", path: "notes.txt", content_base64: Buffer.from(text).toString("base64") },
      ],
    });
    expect((await call("POST", `${repoPath}/commits`, edit("first"))).status).toBe(201);
    expect((await call("POST", `${repoPath}/commits`, edit("second"))).status).toBe(412);
    const notes = await fetch(`${base}/ada/bert-tiny-finetune/resolve/main/notes.txt`, {
      headers: { cookie },
    });
    expect(await notes.text()).toBe("first");
  });

  it("only commits to branches, never to tags or Commit SHAs", async () => {
    const repoPath = `${api}/repos/models/ada/bert-tiny-finetune`;
    const add = (branch: string) => ({
      summary: "Add a file",
      branch,
      operations: [
        { op: "add", path: "tagged.txt", content_base64: Buffer.from("x").toString("base64") },
      ],
    });
    const tag = await call("POST", `${repoPath}/commits`, add("v1.0"));
    expect(tag.status).toBe(400);
    expect(tag.json.detail).toBe("v1.0 is not a branch. Commit to a branch instead.");
    const head = (await call("GET", repoPath)).json.sha as string;
    expect((await call("POST", `${repoPath}/commits`, add(head))).status).toBe(400);
    expect((await call("POST", `${repoPath}/commits`, add("no-such-branch"))).status).toBe(404);
    const refs = await call("GET", `${repoPath}/refs`);
    const branches = refs.json.branches as { name: string }[];
    expect(branches.map((b) => b.name)).toEqual(["main"]);
  });

  it("opens pull requests from Commits and merges them", async () => {
    const pr = await call("POST", `${api}/repos/models/ada/bert-tiny-finetune/commits`, {
      summary: "Fix the card",
      create_pr: true,
      operations: [
        { op: "add", path: "README.md", content_base64: Buffer.from("# New").toString("base64") },
      ],
    });
    expect(pr.status).toBe(201);
    const num = pr.json.pull_request as number;
    const tree = await call(
      "GET",
      `${api}/repos/models/ada/bert-tiny-finetune/tree/${encodeURIComponent(`refs/pr/${num}`)}/README.md`,
    );
    expect(tree.status).toBe(200);
    const merged = await call(
      "POST",
      `${api}/repos/models/ada/bert-tiny-finetune/discussions/${num}/merge`,
      {},
    );
    expect(merged.json.status).toBe("merged");
  });

  it("serves the Dataset Viewer API in the Hub's shapes", async () => {
    const rows = await call(
      "GET",
      "/datasets-server/rows?dataset=ada/imdb-mini&config=default&split=train&offset=100&length=10",
    );
    expect(rows.status).toBe(200);
    expect(rows.json.num_rows_total).toBe(2000);
    expect((rows.json.rows as { row_idx: number }[])[0]?.row_idx).toBe(100);
    const filter = await call(
      "GET",
      `/datasets-server/filter?dataset=ada/imdb-mini&config=default&split=train&where=${encodeURIComponent('"label"=0 AND "rating"<3')}&orderby=${encodeURIComponent('"rating" DESC')}&length=5`,
    );
    const f = filter.json.rows as { row: { label: number; rating: number } }[];
    expect(f.every((r) => r.row.label === 0 && r.row.rating < 3)).toBe(true);
    expect(f[0]?.row.rating).toBe(2);
    expect(
      (
        await call(
          "GET",
          "/datasets-server/filter?dataset=ada/imdb-mini&config=default&split=train&where=nonsense",
        )
      ).status,
    ).toBe(400);
    const search = await call(
      "GET",
      "/datasets-server/search?dataset=ada/imdb-mini&config=default&split=train&query=triumph",
    );
    expect(
      (search.json.rows as { row: { text: string } }[]).every((r) =>
        r.row.text.includes("triumph"),
      ),
    ).toBe(true);
    const stats = await call(
      "GET",
      "/datasets-server/statistics?dataset=ada/imdb-mini&config=default&split=train",
    );
    expect((stats.json.statistics as { column_type: string }[]).map((s) => s.column_type)).toEqual([
      "string_text",
      "class_label",
      "int",
      "struct",
    ]);
    const pending = await call(
      "GET",
      "/datasets-server/rows?dataset=ml-team/speech-commands-mini&config=default&split=train",
    );
    expect(pending.status).toBe(500);
    expect(pending.headers.get("x-error-code")).toBe("ResponseNotReady");
  });

  it("makes Tokens, Collection slugs and audit filters the way the real handlers do", async () => {
    const token = await call("POST", `${api}/me/tokens`, { name: "format-check", scope: "read" });
    expect(token.status).toBe(201);
    // W5's NewTokenSecret: hgr_ and 40 base62 characters.
    expect(token.json.secret).toMatch(/^hgr_[0-9A-Za-z]{40}$/);

    // W7's Slugify, with no suffix; the same title again is a conflict.
    const col = await call("POST", `${api}/collections`, {
      namespace: "ada",
      title: "  Runs on a Laptop!  ",
    });
    expect(col.status).toBe(201);
    expect(col.json.slug).toBe("runs-on-a-laptop");
    const again = await call("POST", `${api}/collections`, {
      namespace: "ada",
      title: "Runs on a laptop",
    });
    expect(again.status).toBe(409);
    expect(
      (await call("POST", `${api}/collections`, { namespace: "ada", title: "!!" })).status,
    ).toBe(400);

    // W5's audit filter matches whole actions, a comma-separated list of them.
    const prefix = await call("GET", `${api}/admin/audit?action=token`);
    expect(prefix.json.items).toEqual([]);
    const exact = await call("GET", `${api}/admin/audit?action=token.created`);
    const created = exact.json.items ?? [];
    expect(created.length).toBeGreaterThan(0);
    expect(created.every((e) => e.action === "token.created")).toBe(true);
    // Creating a Token records an event, as W5's TokenService does.
    expect(created.some((e) => e.target_id === token.json.id)).toBe(true);
    const both = await call("GET", `${api}/admin/audit?action=token.created,user.disabled`);
    expect(new Set(both.json.items?.map((e) => e.action))).toEqual(
      new Set(["token.created", "user.disabled"]),
    );
  });

  it("accepts only invite and reset links an admin made, once each", async () => {
    const invite = await call("POST", `${api}/admin/invites`, {});
    expect(invite.status).toBe(201);
    const url = new URL(String(invite.json.url));
    expect(url.origin).toBe(base);
    const code = url.pathname.split("/").pop() ?? "";
    const accept = (path: string, password = "a-long-enough-password") =>
      call("POST", path, { username: "newcomer", password });
    expect((await accept("/auth/invite/abc123")).status).toBe(404);
    // The link stays usable after a password W5 refuses.
    expect((await accept(`/auth/invite/${code}`, "short")).status).toBe(400);
    expect((await accept(`/auth/invite/${code}`)).status).toBe(201);
    expect((await accept(`/auth/invite/${code}`)).status).toBe(404);
    // Accepting signed the newcomer in; sign ada back in for the next tests.
    await call("POST", `${api}/auth/login`, { username: "ada", password: "correct-horse" });
    // An invite code is not a reset code.
    const second = await call("POST", `${api}/admin/invites`, {});
    const other = new URL(String(second.json.url)).pathname.split("/").pop() ?? "";
    expect((await accept(`/auth/reset/${other}`)).status).toBe(404);
    expect((await accept(`/auth/invite/${other}`)).status).toBe(201);
    await call("POST", `${api}/auth/login`, { username: "ada", password: "correct-horse" });
  });

  it("keeps each browser session's data apart", async () => {
    const own = cookie;
    await call("POST", `${api}/me/tokens`, { name: "isolated", scope: "read" });
    cookie = "";
    await call("POST", `${api}/auth/login`, { username: "ada", password: "correct-horse" });
    const other = await call("GET", `${api}/me/tokens`);
    expect(other.json.items?.some((t) => t.name === "isolated")).toBe(false);
    cookie = own;
    const mine = await call("GET", `${api}/me/tokens`);
    expect(mine.json.items?.some((t) => t.name === "isolated")).toBe(true);
  });
});
