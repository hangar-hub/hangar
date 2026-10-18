// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { createMockCore, type MockOptions, stateBytes } from "./core.ts";
import { createFetchMock, readCapped } from "./fetch.ts";
import { compileValidators, loadSpec } from "./openapi.ts";

// The Fetch API adapter that serves the mock in the demo's Cloudflare Worker
// (demo/worker.ts). playwright.demo.config.ts runs the e2e suite through it.

const { doc, routes } = loadSpec();
const validators = compileValidators(doc);
const MiB = 1024 * 1024;
const api = "http://demo.test/api/hangar/v1";

function mockWith(opts: MockOptions = {}, maxBodyBytes = MiB) {
  return createFetchMock(createMockCore({ routes, validators }, opts), { maxBodyBytes });
}

/** Keeps the mock_session cookie between calls, as a browser does. */
function client(fetchMock: ReturnType<typeof mockWith>) {
  let cookie = "";
  return {
    get cookie() {
      return cookie;
    },
    forget() {
      cookie = "";
    },
    async call(method: string, url: string, init: RequestInit = {}): Promise<Response> {
      const headers = new Headers(init.headers);
      if (cookie) headers.set("Cookie", cookie);
      if (method !== "GET" && method !== "HEAD") headers.set("X-CSRF-Token", "mock-csrf-token");
      const res = await fetchMock(new Request(url, { ...init, method, headers }));
      if (!res) throw new Error(`the mock did not handle ${method} ${url}`);
      for (const c of res.headers.getSetCookie()) {
        const [pair] = c.split(";");
        if (pair?.startsWith("mock_session=")) cookie = pair;
      }
      return res;
    },
  };
}

const json = (body: unknown): RequestInit => ({
  body: JSON.stringify(body),
  headers: { "Content-Type": "application/json" },
});

afterEach(() => {
  vi.useRealTimers();
});

describe("fetch adapter", () => {
  it("answers the native API from the same handlers and keeps every Set-Cookie", async () => {
    const c = client(mockWith());
    // Nobody signed in is not an error for /me, as on Hangar.
    const anonymous = await c.call("GET", `${api}/me`);
    expect(anonymous.status).toBe(204);
    expect(c.cookie).toMatch(/^mock_session=/);

    const login = await c.call(
      "POST",
      `${api}/auth/login`,
      json({ username: "ada", password: "x" }),
    );
    expect(login.status).toBe(200);
    expect(login.headers.getSetCookie()).toContain(
      "hangar_session=mock; Path=/; HttpOnly; SameSite=Lax",
    );
    const me = await c.call("GET", `${api}/me`);
    expect(me.status).toBe(200);
    expect(me.headers.get("content-type")).toBe("application/json");
    expect(await me.json()).toMatchObject({ username: "ada", csrf_token: "mock-csrf-token" });

    const repos = await c.call("GET", `${api}/repos?ns=ada`);
    const body = (await repos.json()) as { items: { name: string }[] };
    expect(body.items.map((r) => r.name)).toContain("bert-tiny-finetune");
  });

  it("validates request bodies against api/openapi.yaml", async () => {
    const c = client(mockWith({ signedIn: true }));
    const res = await c.call("POST", `${api}/repos`, json({ type: "model" }));
    expect(res.status).toBe(400);
    expect(res.headers.get("content-type")).toBe("application/problem+json");
    expect(await res.json()).toMatchObject({ detail: "The request does not match the API." });
  });

  it("returns null for UI pages and assets, which the Worker serves", async () => {
    const fetchMock = mockWith({ signedIn: true });
    expect(await fetchMock(new Request("http://demo.test/models"))).toBeNull();
    expect(await fetchMock(new Request("http://demo.test/assets/index.js"))).toBeNull();
    expect(await fetchMock(new Request("http://demo.test/auth/login"))).toBeNull();
  });

  it("serves resolve downloads with no body for HEAD", async () => {
    const c = client(mockWith({ signedIn: true }));
    const url = "http://demo.test/ada/bert-tiny-finetune/resolve/main/config.json";
    const get = await c.call("GET", url);
    expect(get.status).toBe(200);
    expect(get.headers.get("x-repo-commit")).toMatch(/^[0-9a-f]{40}$/);
    expect((await get.text()).length).toBeGreaterThan(0);
    const head = await c.call("HEAD", url);
    expect(head.status).toBe(200);
    expect(await head.text()).toBe("");
  });

  it("marks the session cookie Secure over HTTPS", async () => {
    const fetchMock = mockWith();
    const res = await fetchMock(new Request("https://demo.test/api/hangar/v1/me"));
    expect(res?.headers.getSetCookie()[0]).toMatch(/; SameSite=Lax; Secure$/);
    const plain = await fetchMock(new Request(`${api}/me`));
    expect(plain?.headers.getSetCookie()[0]).not.toContain("Secure");
  });
});

describe("request body cap", () => {
  it("refuses a declared length over the cap without reading the body", async () => {
    const fetchMock = mockWith({ signedIn: true }, 1024);
    let pulled = false;
    // highWaterMark 0: the stream pulls only when something reads it.
    const body = new ReadableStream<Uint8Array>(
      {
        pull(ctrl) {
          pulled = true;
          ctrl.enqueue(new Uint8Array(10));
          ctrl.close();
        },
      },
      { highWaterMark: 0 },
    );
    const res = await fetchMock(
      new Request(`${api}/repos`, {
        method: "POST",
        body,
        headers: { "Content-Length": "2048", "Content-Type": "application/json" },
        duplex: "half",
      } as RequestInit),
    );
    expect(res?.status).toBe(413);
    expect(res?.headers.get("content-type")).toBe("application/problem+json");
    expect(await res?.json()).toMatchObject({
      status: 413,
      title: "Content Too Large",
      detail: "The demo accepts request bodies up to 1 KiB. Try a smaller file.",
    });
    expect(pulled).toBe(false);
  });

  it("counts the bytes of a body with no length and stops at the cap", async () => {
    const fetchMock = mockWith({ signedIn: true }, MiB);
    const chunk = new Uint8Array(256 * 1024);
    let sent = 0;
    const body = new ReadableStream<Uint8Array>({
      pull(ctrl) {
        // Far more than the cap, so a reader that does not stop never ends.
        if (sent >= 64 * MiB) ctrl.close();
        else {
          sent += chunk.length;
          ctrl.enqueue(chunk);
        }
      },
    });
    const res = await fetchMock(
      new Request(`${api}/repos`, { method: "POST", body, duplex: "half" } as RequestInit),
    );
    expect(res?.status).toBe(413);
    expect(sent).toBeLessThanOrEqual(MiB + 2 * chunk.length);
  });

  it("reads a body up to the cap exactly", async () => {
    const stream = (n: number) =>
      new Response(new Uint8Array(n)).body as ReadableStream<Uint8Array>;
    expect((await readCapped(stream(1024), 1024))?.length).toBe(1024);
    expect(await readCapped(stream(1025), 1024)).toBeNull();
  });
});

describe("session limits", () => {
  // Room for three sessions and what they send here, but not four.
  const limits = { sessionBytes: 4096, totalBytes: 3 * stateBytes + 4096, idleMs: 60_000 };

  it("refuses writes once a session has sent its share", async () => {
    const c = client(mockWith({ signedIn: true, limits }));
    const create = (name: string) =>
      c.call("POST", `${api}/repos`, json({ type: "model", namespace: "ada", name }));
    expect((await create("first")).status).toBe(201);
    const big = await c.call(
      "POST",
      `${api}/repos`,
      json({ type: "model", namespace: "ada", name: "x".repeat(5000) }),
    );
    expect(big.status).toBe(413);
    expect(await big.json()).toMatchObject({
      detail:
        "This session has sent all the data the demo keeps. Clear this site's cookies to start again with the sample data.",
    });
  });

  it("drops the least recently used sessions past the total and starts them again", async () => {
    const fetchMock = mockWith({ signedIn: true, limits });
    const visitors = [client(fetchMock), client(fetchMock), client(fetchMock), client(fetchMock)];
    const first = visitors[0] as ReturnType<typeof client>;
    expect(
      (
        await first.call(
          "POST",
          `${api}/repos`,
          json({ type: "model", namespace: "ada", name: "kept-for-now" }),
        )
      ).status,
    ).toBe(201);
    const has = async (v: ReturnType<typeof client>) =>
      (await v.call("GET", `${api}/repos/models/ada/kept-for-now`)).status;
    expect(await has(first)).toBe(200);
    // Three more visitors: the fourth session pushes the first out.
    for (const v of visitors.slice(1)) await v.call("GET", `${api}/me`);
    const cookie = first.cookie;
    expect(await has(first)).toBe(404);
    // The visitor keeps their cookie and gets the fixtures again.
    expect(first.cookie).toBe(cookie);
  });

  it("drops sessions unused for longer than idleMs", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const fetchMock = mockWith({ signedIn: true, limits: { ...limits, totalBytes: 100 * MiB } });
    const idle = client(fetchMock);
    await idle.call(
      "POST",
      `${api}/repos`,
      json({ type: "model", namespace: "ada", name: "idle" }),
    );
    vi.advanceTimersByTime(limits.idleMs + 1);
    // Another visitor's request prunes the idle session.
    await client(fetchMock).call("GET", `${api}/me`);
    expect((await idle.call("GET", `${api}/repos/models/ada/idle`)).status).toBe(404);
  });
});
