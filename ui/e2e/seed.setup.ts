import { test } from "@playwright/test";
import { seedBulkRepos, signIn } from "./helpers";

// Runs once before the phone, tablet and desktop projects. On a real
// Instance (HANGAR_URL) it makes the data flows share and that takes long to
// create, so the three widths do not all create it at once. The mock keeps
// each browser session's data apart, so there the flows make their own.
test("seeds the Repos the virtualisation flow lists", async ({ page }) => {
  test.setTimeout(10 * 60_000);
  const csrf = await signIn(page);
  await seedBulkRepos(page, csrf);
});
