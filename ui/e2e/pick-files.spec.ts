import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

// Specs choose files with chooseFiles (helpers.ts), which hands the browser
// a file on disk. A buffer passed to setInputFiles is sent to the page as
// base64 and rebuilt there byte by byte: the 20 MiB upload in flows.spec.ts
// took up to 37 seconds that way on a loaded host, and sometimes stalled the
// page until the test timed out.
test("specs choose files through chooseFiles, not setInputFiles", async () => {
  const info = test.info();
  test.skip(info.project.name !== "desktop", "The check reads the specs, not a page.");
  const root = join(info.project.testDir, "..");
  const offenders: string[] = [];
  for (const dir of ["e2e", "e2e-real"]) {
    for (const file of readdirSync(join(root, dir))) {
      if (!file.endsWith(".ts") || (dir === "e2e" && file === "helpers.ts")) continue;
      const lines = readFileSync(join(root, dir, file), "utf8").split("\n");
      lines.forEach((line, i) => {
        if (/\.setInputFiles\(/.test(line)) offenders.push(`${dir}/${file}:${i + 1}`);
      });
    }
  }
  expect(offenders).toEqual([]);
});
