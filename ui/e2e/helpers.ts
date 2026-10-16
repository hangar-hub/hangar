import AxeBuilder from "@axe-core/playwright";
import {
  type APIResponse,
  expect,
  type Locator,
  type Page,
  type Request,
  type Response,
} from "@playwright/test";

/**
 * Signs in through the API before the first page load, so the page starts
 * signed in. Returns the session's CSRF token, for `callApi`.
 */
export async function signIn(page: Page, username = "ada"): Promise<string> {
  const res = await page.request.post("/api/hangar/v1/auth/login", {
    data: { username, password: "correct-horse-battery" },
  });
  expect(res.ok()).toBe(true);
  const me = (await res.json()) as { csrf_token?: string };
  expect(me.csrf_token).toBeTruthy();
  return me.csrf_token ?? "";
}

/**
 * Calls the native API in the page's session, to set up what a flow needs
 * on any Instance: the mock or a real one.
 */
export function callApi(
  page: Page,
  csrf: string,
  method: "GET" | "POST" | "PATCH" | "DELETE",
  path: string,
  data?: unknown,
): Promise<APIResponse> {
  return page.request.fetch(`/api/hangar/v1${path}`, {
    method,
    data,
    headers: method === "GET" ? {} : { "X-CSRF-Token": csrf },
  });
}

/**
 * A short suffix for names a flow creates, so the flows can run again, and
 * at three widths at once, against the same Instance.
 */
export function unique(): string {
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
}

/** How many Repos `seedBulkRepos` makes: more than the 200 the arsenal virtualises past. */
export const bulkRepoCount = 260;

/**
 * Makes the Organisation `bulk` with Repos `bulk/bulk-model-000` onwards,
 * creating only the ones that are missing, so it is quick once a seed run
 * has made them.
 */
export async function seedBulkRepos(page: Page, csrf: string): Promise<void> {
  const org = await callApi(page, csrf, "POST", "/orgs", { name: "bulk", display_name: "Bulk" });
  expect([201, 409], await org.text()).toContain(org.status());
  const have = new Set<string>();
  for (let cursor: string | undefined, first = true; first || cursor; first = false) {
    const q = new URLSearchParams({ ns: "bulk", sort: "name", limit: "500" });
    if (cursor) q.set("cursor", cursor);
    const res = await callApi(page, csrf, "GET", `/repos?${q}`);
    expect(res.ok(), await res.text()).toBe(true);
    const body = (await res.json()) as { items: { name: string }[]; next_cursor?: string };
    for (const r of body.items) have.add(r.name);
    cursor = body.next_cursor;
  }
  const missing = Array.from(
    { length: bulkRepoCount },
    (_, i) => `bulk-model-${String(i).padStart(3, "0")}`,
  ).filter((n) => !have.has(n));
  // A few at a time: Repo creation runs git, and a real Instance shares
  // the host with the browsers.
  const create = async () => {
    for (let name = missing.shift(); name; name = missing.shift()) {
      const res = await callApi(page, csrf, "POST", "/repos", {
        type: "model",
        namespace: "bulk",
        name,
      });
      expect([201, 409], `creating bulk/${name}: ${await res.text()}`).toContain(res.status());
    }
  };
  await Promise.all(Array.from({ length: 4 }, create));
}

const byteUnits: Record<string, number> = {
  B: 1,
  KiB: 1024,
  MiB: 1024 ** 2,
  GiB: 1024 ** 3,
  TiB: 1024 ** 4,
};

/** Reads a size the UI formats with formatBytes, such as "1.1 GiB", back into bytes. */
export function parseBytes(text: string): number {
  const m = /([\d.]+)\s*(B|KiB|MiB|GiB|TiB)\b/.exec(text);
  if (!m) throw new Error(`No size in "${text}".`);
  return Number(m[1]) * (byteUnits[m[2] ?? "B"] ?? 1);
}

/**
 * Opens the search palette with `/` and types a query, as a keyboard user
 * does: the palette must open with focus in its input.
 */
export async function searchWithKeyboard(page: Page, query: string): Promise<Locator> {
  const dialog = page.getByRole("dialog", { name: "Search" });
  // The first press can come before the page listens for keys, so press
  // again until the palette opens, but never once it is open.
  await expect(async () => {
    if (!(await dialog.isVisible())) await page.keyboard.press("/");
    await expect(dialog).toBeVisible({ timeout: 1000 });
  }).toPass();
  const input = dialog.getByRole("combobox", { name: "Search Hangar" });
  await expect(input).toBeFocused();
  // A press that raced the palette opening typed "/" into the input.
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.type(query);
  await expect(input).toHaveValue(query);
  return dialog;
}

/**
 * Waits for the page's next Commit to a Repo, such as
 * "models/ada/bert-tiny-finetune". A Commit runs git on the server, which can
 * take seconds on a loaded host, so flows wait for the Commit itself rather
 * than for a fixed time before they look for its result.
 */
export function commitResponse(page: Page, repo: string): Promise<Response> {
  return page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname === `/api/hangar/v1/repos/${repo}/commits`,
  );
}

/**
 * Waits for the page's next answer to a pull request's file diff in a Repo,
 * such as "models/ada/bert-tiny-finetune". The diff runs several git
 * processes on the server (both heads, their fork point and the diff), which
 * can take longer than an assertion's 5 seconds on a loaded host, so flows
 * wait for the diff itself before they look for it on the page.
 */
export function pullRequestDiffResponse(page: Page, repo: string): Promise<Response> {
  const path = new RegExp(`^/api/hangar/v1/repos/${repo}/discussions/\\d+/diff$`);
  return page.waitForResponse(
    (r) => r.request().method() === "GET" && path.test(new URL(r.url()).pathname),
  );
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

/** True for a request the page sent to the Hangar API. */
function isApiRequest(r: Request): boolean {
  return new URL(r.url()).pathname.startsWith("/api/");
}

/**
 * Resolves once a request has its answer, or has failed. Hangar sends an API
 * answer's headers when it has worked out the whole answer, so the body
 * follows at once. Response.finished() would not do: it never resolves for a
 * response that finished before the test asked for it.
 */
async function answered(r: Request): Promise<void> {
  await r.response();
}

/**
 * Waits until no loading placeholder is on the page. A placeholder stands for
 * an API request the page is waiting on, and on a loaded host the git behind
 * some reads, such as a Repo's Commits, takes more than 10 seconds. So
 * settled() first waits for every API request the page has sent to be
 * answered, for as long as the test's timeout allows, and then gives the
 * placeholders 10 seconds to clear. A placeholder still there when no request
 * is left to answer is a page stuck loading, and fails the test.
 */
export async function settled(page: Page): Promise<void> {
  const placeholders = page.locator('[role="status"][aria-label^="Loading"]');
  const waited = new Set<Request>();
  const notWaitedFor = async () =>
    (await page.requests()).filter((r) => isApiRequest(r) && !waited.has(r));
  for (;;) {
    const sent = await notWaitedFor();
    for (const r of sent) waited.add(r);
    await Promise.all(sent.map(answered));
    try {
      await expect(placeholders).toHaveCount(0, { timeout: 10_000 });
      return;
    } catch (e) {
      // The page sent more requests while the placeholders were checked:
      // wait for those too.
      if ((await notWaitedFor()).length === 0) throw e;
    }
  }
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
