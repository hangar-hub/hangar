import { gzipSync } from "node:zlib";
import { expect, test } from "@playwright/test";
import { signIn } from "./helpers";

// 09-web-ui.md "Performance": initial JavaScript under 250 KB gzipped, with
// routes split by page. "Initial" is every script the page requests before
// its heading appears. Markdown, syntax highlighting and other content
// libraries load after that, when the content that needs them arrives.
const budget = 250 * 1024;

for (const [name, path, auth] of [
  ["landing page", "/", false],
  ["Arsenal", "/arsenal", true],
  ["Repo page", "/ada/bert-tiny-finetune", true],
  ["dataset Viewer", "/datasets/ada/imdb-mini/viewer/default/train", true],
] as const) {
  test(`the ${name} starts with under 250 KB of gzipped JavaScript`, async ({ page }, info) => {
    test.skip(info.project.name !== "desktop", "The budget does not depend on the viewport.");
    if (auth) await signIn(page);
    // Record, in the page, when the first h1 appears.
    await page.addInitScript(() => {
      const w = window as unknown as { __h1At?: number };
      new MutationObserver((_, obs) => {
        if (document.querySelector("h1")) {
          w.__h1At = performance.now();
          obs.disconnect();
        }
      }).observe(document, { childList: true, subtree: true });
    });
    const sizes: Record<string, number> = {};
    const pending: Promise<void>[] = [];
    page.on("response", (res) => {
      if (res.request().resourceType() !== "script") return;
      pending.push(
        res
          .body()
          .then((body) => {
            sizes[new URL(res.url()).pathname] = gzipSync(body).length;
          })
          .catch(() => undefined),
      );
    });
    await page.goto(path);
    await expect(page.locator("h1")).toBeVisible();
    await page.waitForLoadState("networkidle");
    await Promise.all(pending);
    const early = await page.evaluate(() => {
      const at = (window as unknown as { __h1At?: number }).__h1At ?? Number.POSITIVE_INFINITY;
      return performance
        .getEntriesByType("resource")
        .filter(
          (e) =>
            (e as PerformanceResourceTiming).initiatorType === "script" || e.name.endsWith(".js"),
        )
        .filter((e) => e.startTime <= at)
        .map((e) => new URL(e.name).pathname);
    });
    const initial = Object.fromEntries(
      Object.entries(sizes).filter(([path]) => early.includes(path)),
    );
    const total = Object.values(initial).reduce((a, n) => a + n, 0);
    expect(Object.keys(initial).length).toBeGreaterThan(0);
    expect(total, JSON.stringify(initial, null, 2)).toBeLessThan(budget);
  });
}
