import { expect, test } from "@playwright/test";
import {
  callApi,
  expectAccessible,
  expectNoHorizontalOverflow,
  expectTheme,
  settled,
  unique,
  watchConsole,
} from "../e2e/helpers";

// The demo at demo.hangar.page, under wrangler dev with the settings it is
// deployed with (demo/wrangler.jsonc): the banner, a visitor signed in from
// the start, the Content Security Policy, and the limits that keep anyone
// from using it to store files (demo/worker.ts).

const MiB = 1024 * 1024;

test("visitors start signed in and see the demo banner with a link to the docs", async ({
  page,
}) => {
  const errors = watchConsole(page);
  await page.goto("/models");
  const banner = page.getByRole("region", { name: "Demo" });
  await expect(banner).toHaveText(
    "This is a demo with sample data. Nothing you do here is saved. Read the documentation",
  );
  await expect(banner.getByRole("link", { name: "Read the documentation" })).toHaveAttribute(
    "href",
    "https://hangar.page/",
  );
  await expect(page.getByRole("button", { name: "Account menu for ada" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await settled(page);
  await expectAccessible(page);
  await expectNoHorizontalOverflow(page);
  expect(errors).toEqual([]);
});

test("the banner passes axe in dark mode and fits a phone", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto("/");
  await expectTheme(page, "dark");
  await expect(page.getByRole("region", { name: "Demo" })).toBeVisible();
  await settled(page);
  await expectAccessible(page);
  await expectNoHorizontalOverflow(page);
});

test("pages carry the Go server's security headers and a fresh style nonce", async ({
  request,
}) => {
  const nonces = new Set<string>();
  for (let i = 0; i < 2; i++) {
    const res = await request.get("/ada/bert-tiny-finetune", {
      headers: { Accept: "text/html" },
    });
    expect(res.status()).toBe(200);
    const h = res.headers();
    const nonce = /<meta name="csp-nonce" content="([^"]+)"/.exec(await res.text())?.[1] ?? "";
    expect(nonce).not.toBe("");
    nonces.add(nonce);
    expect(h["content-security-policy"]).toBe(
      [
        "default-src 'self'",
        "script-src 'self'",
        `style-src 'self' 'nonce-${nonce}'`,
        "img-src 'self' data: blob: https:",
        "media-src 'self' blob:",
        "font-src 'self'",
        "connect-src 'self'",
        "worker-src 'self' blob:",
        "object-src 'none'",
        "base-uri 'self'",
        "form-action 'self'",
        "frame-ancestors 'none'",
      ].join("; "),
    );
    expect(h["x-frame-options"]).toBe("DENY");
    expect(h["x-content-type-options"]).toBe("nosniff");
    expect(h["cross-origin-opener-policy"]).toBe("same-origin");
    expect(h["referrer-policy"]).toBe("strict-origin-when-cross-origin");
    expect(h["permissions-policy"]).toContain("camera=()");
    expect(h["x-robots-tag"]).toBe("noindex");
  }
  expect(nonces.size).toBe(2);
  const asset = await request.get("/assets/hangar.svg");
  expect(asset.status()).toBe(200);
  expect(asset.headers()["x-content-type-options"]).toBe("nosniff");
});

test("request bodies over 1 MiB get 413 and smaller ones work", async ({ page }) => {
  await page.goto("/");
  const me = await page.request.get("/api/hangar/v1/me");
  const csrf = ((await me.json()) as { csrf_token: string }).csrf_token;
  const name = `cap-${unique()}`;
  const created = await callApi(page, csrf, "POST", "/repos", {
    type: "model",
    namespace: "ada",
    name,
  });
  expect(created.status(), await created.text()).toBe(201);

  const commit = (bytes: number) =>
    callApi(page, csrf, "POST", `/repos/models/ada/${name}/commits`, {
      summary: "Add a file",
      operations: [
        {
          op: "add",
          path: "notes.txt",
          content_base64: Buffer.alloc(bytes, "a").toString("base64"),
        },
      ],
    });
  const large = await commit(MiB);
  expect(large.status()).toBe(413);
  expect(((await large.json()) as { detail: string }).detail).toBe(
    "The demo accepts request bodies up to 1 MiB. Try a smaller file.",
  );
  const small = await commit(64 * 1024);
  expect(small.status(), await small.text()).toBe(201);
});

test("the OIDC sign-in redirects only to paths on the demo", async ({ request }) => {
  const location = async (to: string) =>
    (
      await request.get(`/auth/oidc/unifi?redirect=${encodeURIComponent(to)}`, { maxRedirects: 0 })
    ).headers().location;
  expect(await location("/datasets")).toBe("/datasets");
  for (const evil of ["https://evil.example/", "//evil.example/", "/\\evil.example/"]) {
    expect(await location(evil), evil).toBe("/");
  }
});

test("one visitor's changes never reach another", async ({ browser }) => {
  const visitor = async () => {
    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto("/");
    const me = await page.request.get("/api/hangar/v1/me");
    const csrf = ((await me.json()) as { csrf_token: string }).csrf_token;
    return { context, page, csrf };
  };
  const a = await visitor();
  const b = await visitor();
  const name = `planted-${unique()}`;
  const planted = await callApi(a.page, a.csrf, "POST", "/repos", {
    type: "model",
    namespace: "ada",
    name,
  });
  expect(planted.status()).toBe(201);
  const renamed = await callApi(a.page, a.csrf, "PATCH", "/orgs/ml-team", {
    display_name: "Click this link",
  });
  expect(renamed.status()).toBe(200);
  expect((await callApi(a.page, a.csrf, "POST", "/auth/logout")).ok()).toBe(true);

  expect((await callApi(b.page, b.csrf, "GET", `/repos/models/ada/${name}`)).status()).toBe(404);
  await b.page.goto("/ml-team");
  await expect(b.page.getByRole("heading", { level: 1 })).toHaveText("ML team");
  await expect(b.page.getByRole("button", { name: "Account menu for ada" })).toBeVisible();
  await a.context.close();
  await b.context.close();
});
