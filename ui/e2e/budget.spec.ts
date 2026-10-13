import { gzipSync } from "node:zlib";
import { expect, test } from "@playwright/test";
import { signIn } from "./helpers";

// 09-web-ui.md "Performance": initial JavaScript under 250 KB gzipped, with
// routes split by page. "Initial" is every script the page loads before its
// heading shows. Markdown, syntax highlighting and other content libraries
// load after that, when the content that needs them arrives.
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
    await Promise.all(pending);
    const initial = { ...sizes };
    const total = Object.values(initial).reduce((a, n) => a + n, 0);
    expect(Object.keys(initial).length).toBeGreaterThan(0);
    expect(total, JSON.stringify(initial, null, 2)).toBeLessThan(budget);
  });
}
