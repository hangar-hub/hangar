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

/**
 * Runs every axe rule, as phase 0's shell test did: the WCAG 2.2 AA rules
 * (09-web-ui.md "Accessibility") and axe's best practices, such as heading
 * order.
 */
export async function expectAccessible(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page }).analyze();
  const summary = results.violations.map(
    (v) =>
      `${v.id}: ${v.help} (${v.nodes
        .map((n) => n.target.join(" "))
        .slice(0, 3)
        .join(", ")})`,
  );
  expect(summary).toEqual([]);
}

/**
 * Fails when the page's text says "Owner", which CONTEXT.md says to avoid:
 * the UI calls the owner part of a Repo ID a Namespace.
 */
export async function expectGlossaryTerms(page: Page): Promise<void> {
  const text = await page.locator("body").innerText();
  expect(text.match(/\bowners?\b/gi) ?? []).toEqual([]);
}

/** Waits until no loading placeholder is on the page. */
export async function settled(page: Page): Promise<void> {
  await expect(page.locator('[role="status"][aria-label^="Loading"]')).toHaveCount(0, {
    timeout: 10_000,
  });
}

/**
 * Fails when the page scrolls sideways: 09-web-ui.md asks for layouts that
 * fit phone, tablet and desktop widths, so only regions built to scroll (code,
 * tables) may be wider than the viewport. Names the widest elements that
 * stick out, to say where to look.
 */
export async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  const viewport = page.viewportSize();
  if (!viewport) throw new Error("The page has no viewport size.");
  const result = await page.evaluate((width) => {
    const describe = (el: Element) => {
      const id = el.id ? `#${el.id}` : "";
      const cls =
        typeof el.className === "string" && el.className
          ? `.${el.className.trim().split(/\s+/).slice(0, 3).join(".")}`
          : "";
      return `${el.tagName.toLowerCase()}${id}${cls} (right edge ${Math.round(el.getBoundingClientRect().right)} px)`;
    };
    const sticking = [...document.body.querySelectorAll("*")].filter((el) => {
      if (el.getBoundingClientRect().right <= width + 1) return false;
      // Report the outermost element that sticks out, not each descendant
      // inside a region that already scrolls.
      for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
        const s = getComputedStyle(p);
        if (s.overflowX !== "visible") return false;
        if (p.getBoundingClientRect().right > width + 1) return false;
      }
      return true;
    });
    return {
      scrollWidth: document.documentElement.scrollWidth,
      offenders: sticking.slice(0, 3).map(describe),
    };
  }, viewport.width);
  expect(
    result.scrollWidth,
    `the page is ${result.scrollWidth} px wide in a ${viewport.width} px viewport; sticking out: ${result.offenders.join(", ")}`,
  ).toBeLessThanOrEqual(viewport.width);
}
