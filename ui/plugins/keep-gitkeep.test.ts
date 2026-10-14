// @vitest-environment node
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { build } from "vite";
import { afterEach, describe, expect, it } from "vitest";
import { keepGitkeep } from "./keep-gitkeep";

describe("keepGitkeep", () => {
  let root = "";
  afterEach(() => {
    if (root) rmSync(root, { recursive: true, force: true });
  });

  it("puts .gitkeep back after emptyOutDir clears the output", async () => {
    root = mkdtempSync(join(tmpdir(), "hangar-keep-gitkeep-"));
    writeFileSync(
      join(root, "index.html"),
      '<!doctype html><script type="module" src="./main.js"></script>',
    );
    writeFileSync(join(root, "main.js"), "document.title = 'built';\n");
    const out = join(root, "dist");
    mkdirSync(out);
    writeFileSync(join(out, ".gitkeep"), "");
    writeFileSync(join(out, "stale.js"), "old build");

    await build({
      root,
      configFile: false,
      logLevel: "silent",
      plugins: [keepGitkeep()],
      build: { outDir: "dist", emptyOutDir: true },
    });

    expect(existsSync(join(out, "stale.js"))).toBe(false);
    expect(existsSync(join(out, "index.html"))).toBe(true);
    expect(readFileSync(join(out, ".gitkeep"), "utf8")).toBe("");
  });
});
