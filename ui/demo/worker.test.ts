// @vitest-environment node
import { describe, expect, it } from "vitest";
import { compileValidators, loadSpec } from "../mock/openapi.ts";
import { createDemoWorker, type Env, indexedHead, maxBodyBytes } from "./worker.ts";

// The demo Worker with a stand-in for the static assets binding. The
// Playwright suite runs the real one under wrangler dev
// (playwright.demo.config.ts).

const { doc } = loadSpec();
const index = "<!doctype html><html><head><title>Hangar</title></head><body></body></html>";

function env(vars: Partial<Env> = {}): Env & { asked: string[] } {
  const asked: string[] = [];
  return {
    asked,
    ASSETS: {
      async fetch(request: Request) {
        const path = new URL(request.url).pathname;
        asked.push(path);
        if (path === "/") return new Response(index, { headers: { "Content-Type": "text/html" } });
        if (path === "/assets/app.js")
          return new Response("console.log(1)", { headers: { "Content-Type": "text/javascript" } });
        return new Response("Not found", { status: 404 });
      },
    },
    ...vars,
  };
}

const worker = () => createDemoWorker(doc, compileValidators(doc));
const page = (path: string) =>
  new Request(`https://demo.hangar.page${path}`, { headers: { Accept: "text/html" } });

describe("demo Worker", () => {
  it("serves index.html for UI routes with a nonce in the page and the CSP", async () => {
    const e = env();
    const res = await worker().fetch(page("/ada/bert-tiny-finetune"), e);
    expect(res.status).toBe(200);
    expect(e.asked).toEqual(["/"]);
    const html = await res.text();
    const nonce = /<meta name="csp-nonce" content="([^"]+)" \/>/.exec(html)?.[1];
    expect(nonce).toMatch(/^[A-Za-z0-9+/]{22}==$/);
    expect(res.headers.get("content-security-policy")).toContain(
      `style-src 'self' 'nonce-${nonce}'`,
    );
    expect(res.headers.get("content-security-policy")).toContain("frame-ancestors 'none'");
    expect(res.headers.get("x-frame-options")).toBe("DENY");
    expect(res.headers.get("cache-control")).toBe("no-cache");
    expect(res.headers.get("x-robots-tag")).toBe("noindex");
  });

  it("starts visitors signed in, or signed out with DEMO_SIGNED_IN=0", async () => {
    const me = new Request("https://demo.hangar.page/api/hangar/v1/me");
    const signedIn = await worker().fetch(me, env());
    expect(signedIn.status).toBe(200);
    expect(await signedIn.json()).toMatchObject({ username: "ada" });
    expect(signedIn.headers.get("x-content-type-options")).toBe("nosniff");
    const signedOut = await worker().fetch(me, env({ DEMO_SIGNED_IN: "0" }));
    expect(signedOut.status).toBe(204);
  });

  it("refuses bodies over 1 MiB", async () => {
    const res = await worker().fetch(
      new Request("https://demo.hangar.page/api/hangar/v1/repos", {
        method: "POST",
        body: new Uint8Array(maxBodyBytes + 1),
        headers: { "Content-Type": "application/json", "X-CSRF-Token": "mock-csrf-token" },
      }),
      env(),
    );
    expect(maxBodyBytes).toBe(1024 * 1024);
    expect(res.status).toBe(413);
  });

  it("raises the body cap only when DEMO_TEST_LIMIT_MIB is set", async () => {
    const post = () =>
      new Request("https://demo.hangar.page/api/hangar/v1/repos", {
        method: "POST",
        body: JSON.stringify({
          type: "model",
          namespace: "ada",
          name: "x".repeat(2 * 1024 * 1024),
        }),
        headers: { "Content-Type": "application/json", "X-CSRF-Token": "mock-csrf-token" },
      });
    expect((await worker().fetch(post(), env())).status).toBe(413);
    // Past the cap, the mock reads the body and answers it.
    expect((await worker().fetch(post(), env({ DEMO_TEST_LIMIT_MIB: "64" }))).status).not.toBe(413);
  });

  it("lets search engines index only the home page on demo.hangar.page", async () => {
    const home = await worker().fetch(page("/?utm_source=x"), env());
    expect(home.headers.get("x-robots-tag")).toBeNull();
    const html = await home.text();
    expect(html).toContain(indexedHead);
    expect(html).not.toContain("<title>Hangar</title>");

    // Googlebot asks with Accept: */* and must get the same page.
    for (const accept of ["*/*", ""]) {
      const crawl = await worker().fetch(
        new Request("https://demo.hangar.page/", { headers: accept ? { Accept: accept } : {} }),
        env(),
      );
      expect(crawl.headers.get("x-robots-tag")).toBeNull();
      expect(await crawl.text()).toContain(indexedHead);
    }

    const preview = await worker().fetch(
      new Request("https://main.demo.hangar.page/", { headers: { Accept: "text/html" } }),
      env(),
    );
    expect(preview.headers.get("x-robots-tag")).toBe("noindex");
    expect(await preview.text()).toContain("<title>Hangar</title>");

    const repo = await worker().fetch(page("/ada/bert-tiny-finetune"), env());
    expect(await repo.text()).not.toContain(indexedHead);
    const me = await worker().fetch(
      new Request("https://demo.hangar.page/api/hangar/v1/me"),
      env(),
    );
    expect(me.headers.get("x-robots-tag")).toBe("noindex");
  });

  it("serves the search engines' verification files at the root", async () => {
    const files = { "/BingSiteAuth.xml": "<users />", "/google1.html": "google-site-verification" };
    const demo = createDemoWorker(doc, compileValidators(doc), files);
    for (const [path, body] of Object.entries(files)) {
      const res = await demo.fetch(page(path), env());
      expect(res.status).toBe(200);
      expect(await res.text()).toBe(body);
    }
    const xml = await demo.fetch(new Request("https://demo.hangar.page/BingSiteAuth.xml"), env());
    expect(xml.headers.get("content-type")).toBe("application/xml");
    const other = await demo.fetch(page("/google2.html"), env());
    expect(await other.text()).toContain("<title>");
  });

  it("passes assets through with the common headers and refuses other methods", async () => {
    const asset = await worker().fetch(
      new Request("https://demo.hangar.page/assets/app.js"),
      env(),
    );
    expect(await asset.text()).toBe("console.log(1)");
    expect(asset.headers.get("x-content-type-options")).toBe("nosniff");
    expect(asset.headers.get("referrer-policy")).toBe("strict-origin-when-cross-origin");
    const post = await worker().fetch(
      new Request("https://demo.hangar.page/models", { method: "POST" }),
      env(),
    );
    expect(post.status).toBe(405);
    expect(post.headers.get("allow")).toBe("GET, HEAD");
  });
});
