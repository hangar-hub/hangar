import AxeBuilder from "@axe-core/playwright";
import { expect, type Page } from "@playwright/test";

/** Signs in through the API before the first page load, so the page starts signed in. */
export async function signIn(page: Page, username = "ada"): Promise<void> {
  const res = await page.request.post("/api/hangar/v1/auth/login", {
    data: { username, password: "correct-horse-battery" },
  });
  expect(res.ok()).toBe(true);
}

/** Uses a mock scenario (`empty` or `bulk`) for this browser context. */
export async function useScenario(
  page: Page,
  scenario: "empty" | "bulk",
  baseURL: string,
): Promise<void> {
  await page.context().addCookies([{ name: "mock_scenario", value: scenario, url: baseURL }]);
}

/** Fails the test on Content Security Policy violations and uncaught errors. */
export function watchConsole(page: Page): string[] {
  const problems: string[] = [];
  page.on("console", (m) => {
    const t = m.text();
    if (m.type() === "error" && /Content Security Policy|Refused to/i.test(t)) problems.push(t);
  });
  page.on("pageerror", (e) => problems.push(`Uncaught: ${e.message}`));
  return problems;
}

/** Runs axe with the WCAG 2.2 AA rules (09-web-ui.md "Accessibility"). */
export async function expectAccessible(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  const summary = results.violations.map(
    (v) =>
      `${v.id}: ${v.help} (${v.nodes
        .map((n) => n.target.join(" "))
        .slice(0, 3)
        .join(", ")})`,
  );
  expect(summary).toEqual([]);
}

/** Waits until no loading placeholder is on the page. */
export async function settled(page: Page): Promise<void> {
  await expect(page.locator('[role="status"][aria-label^="Loading"]')).toHaveCount(0, {
    timeout: 10_000,
  });
}
