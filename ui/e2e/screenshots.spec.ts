import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";
import { settled, signIn } from "./helpers";

// Screenshots of the main pages at phone, tablet and desktop widths for
// docs/assets/screenshots/. Run with `pnpm run screenshots`; they are
// converted to WebP with cwebp to keep the repository small.
const out = fileURLToPath(new URL("../../docs/assets/screenshots/", import.meta.url));

const shots: { name: string; path: string; signedIn: boolean }[] = [
  { name: "landing", path: "/", signedIn: false },
  { name: "arsenal", path: "/arsenal", signedIn: true },
  { name: "discover", path: "/discover?repo=TinyLlama/TinyLlama-1.1B-Chat-GGUF", signedIn: true },
  { name: "model-card", path: "/ada/bert-tiny-finetune", signedIn: true },
  { name: "files-mirror", path: "/TinyLlama/TinyLlama-1.1B-Chat-GGUF/tree/main", signedIn: true },
  {
    name: "gguf-viewer",
    path: "/TinyLlama/TinyLlama-1.1B-Chat-GGUF/blob/main/tinyllama-1.1b-chat.Q4_K_M.gguf",
    signedIn: true,
  },
  { name: "dataset-viewer", path: "/datasets/ada/imdb-mini/viewer/default/train", signedIn: true },
  { name: "tokens", path: "/settings/tokens", signedIn: true },
  { name: "admin-system", path: "/admin/system", signedIn: true },
];

test.describe("screenshots", () => {
  test.skip(!process.env.SCREENSHOTS, "Set SCREENSHOTS=1 to refresh docs/assets/screenshots.");

  for (const shot of shots) {
    test(shot.name, async ({ page }, info) => {
      mkdirSync(out, { recursive: true });
      if (shot.signedIn) await signIn(page);
      await page.goto(shot.path);
      await expect(page.locator("h1")).toHaveCount(1);
      await settled(page);
      // Let lazily loaded content (Markdown, highlighting) and fonts settle.
      await page.waitForLoadState("networkidle");
      const png = `${out}${shot.name}-${info.project.name}.png`;
      await page.screenshot({ path: png, fullPage: false, animations: "disabled" });
      try {
        execFileSync("cwebp", ["-quiet", "-q", "80", png, "-o", png.replace(/\.png$/, ".webp")]);
        rmSync(png);
      } catch {
        // Without cwebp the PNG stays; it is still committed compressed by git.
      }
    });
  }
});
