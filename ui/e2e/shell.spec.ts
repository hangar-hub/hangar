import AxeBuilder from "@axe-core/playwright";
import { expect, test } from "@playwright/test";

// Every page shell renders inside the layout and passes axe. W8 extends
// these into the flows in 13-testing.md.
const pages = [
  "/",
  "/arsenal",
  "/discover",
  "/collections",
  "/auth/login",
  "/settings/tokens",
  "/admin/system",
];

for (const path of pages) {
  test(`${path} renders and is accessible`, async ({ page }) => {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
    const results = await new AxeBuilder({ page }).analyze();
    expect(results.violations).toEqual([]);
  });
}
