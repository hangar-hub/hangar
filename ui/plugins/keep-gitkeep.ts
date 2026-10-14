import { mkdirSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import type { Plugin } from "vite";

/**
 * Puts an empty `.gitkeep` back in the build output after every build.
 *
 * The Go binary embeds internal/web/dist, so the directory is tracked with a
 * `.gitkeep` in it. `emptyOutDir` deletes that file on every `vite build`,
 * including the one Playwright runs, which leaves the worktree dirty.
 */
export function keepGitkeep(): Plugin {
  let outDir = "";
  return {
    name: "hangar-keep-gitkeep",
    apply: "build",
    configResolved(config) {
      outDir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      mkdirSync(outDir, { recursive: true });
      writeFileSync(join(outDir, ".gitkeep"), "");
    },
  };
}
