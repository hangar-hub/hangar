import { createHash } from "node:crypto";
import { expect, test } from "@playwright/test";
import {
  callApi,
  chooseFiles,
  expectAccessible,
  expectGlossaryTerms,
  expectNoHorizontalOverflow,
  expectTheme,
  searchWithKeyboard,
  settled,
  signIn,
  unique,
  watchConsole,
  watchFailedLoads,
} from "./helpers";

// Every page in 09-web-ui.md renders, has one h1, passes every axe rule,
// avoids the glossary's "Avoid" terms and fits the viewport without scrolling
// sideways, at phone (390 px), tablet and desktop widths.

/** A Commit SHA in the mock's fixture data (ui/mock/data.ts makeCommits). */
const commitSha = (repo: string, title: string) =>
  createHash("sha1").update(`${repo}:${title}`).digest("hex");

const signedIn = [
  "/",
  "/models",
  "/models?view=list&sort=parameters",
  "/datasets",
  "/datasets?view=list",
  "/discover",
  "/discover?type=datasets",
  "/discover?repo=TinyLlama/TinyLlama-1.1B-Chat-GGUF",
  "/ada/bert-tiny-finetune",
  "/ada/bert-tiny-finetune/tree/main",
  "/ada/bert-tiny-finetune/tree/main/onnx",
  "/ada/bert-tiny-finetune/blob/main/README.md",
  "/ada/bert-tiny-finetune/blob/main/config.json",
  "/ada/bert-tiny-finetune/blob/main/model.safetensors",
  "/ada/legacy-checkpoint/blob/main/model.ckpt",
  "/TinyLlama/TinyLlama-1.1B-Chat-GGUF/blob/main/tinyllama-1.1b-chat.Q4_K_M.gguf",
  "/ada/bert-tiny-finetune/commits/main",
  `/ada/bert-tiny-finetune/commit/${commitSha("ada/bert-tiny-finetune", "Add safetensors weights and ONNX export")}`,
  "/ada/bert-tiny-finetune/discussions",
  "/ada/bert-tiny-finetune/discussions/2",
  "/ada/bert-tiny-finetune/settings",
  "/TinyLlama/TinyLlama-1.1B-Chat-GGUF/settings",
  "/datasets/ada/imdb-mini",
  "/datasets/ada/imdb-mini/viewer/default/train",
  "/datasets/ada/cifar-mini/viewer/default/train",
  "/datasets/ml-team/speech-commands-mini/viewer",
  "/datasets/ada/imdb-mini/tree/main",
  "/datasets/ada/imdb-mini/blob/main/data/train.csv",
  "/datasets/ada/imdb-mini/commits/main",
  `/datasets/ada/imdb-mini/commit/${commitSha("ada/imdb-mini", "Add train and test splits")}`,
  "/datasets/ada/imdb-mini/discussions",
  "/datasets/ada/imdb-mini/settings",
  "/datasets/ada/ingest-samples",
  "/datasets/ada/ingest-samples/viewer",
  "/datasets/ada/ingest-samples/tree/main/fixtures",
  "/new",
  "/new-dataset",
  "/ada",
  "/ml-team?tab=members",
  "/collections",
  "/collections/ada/small-and-fast",
  "/settings",
  "/settings/profile",
  "/settings/tokens",
  "/settings/ollama-keys",
  "/settings/upstream-tokens",
  "/settings/sessions",
  "/settings/organisations",
  "/settings/device?code=ABCD-1234",
  "/admin",
  "/admin/system",
  "/admin/users",
  "/admin/organisations",
  "/admin/organisations?q=ml",
  "/admin/upstreams",
  "/admin/pulls",
  "/admin/storage",
  "/admin/jobs",
  "/admin/webhooks",
  "/admin/audit",
  "/admin/settings",
  "/no-such-namespace",
  // Not-found states with long IDs that have no spaces or hyphens to wrap at.
  `/ada/${"m".repeat(90)}`,
  `/ada/bert-tiny-finetune/blob/main/${"a".repeat(120)}.txt`,
];

const signedOut = [
  "/",
  "/auth/login",
  "/auth/invite/abc123",
  "/auth/reset/abc123",
  "/auth/email/abc123",
  "/auth/login?error=oidc_denied",
];

// Spec 09 asks for axe in both themes on every page. The light theme runs
// every check; the dark theme runs axe, which is what changes with the
// palette, on the same pages.
const schemes = ["light", "dark"] as const;

for (const path of signedIn) {
  test(`${path} renders, fits the width and is accessible`, async ({ page }) => {
    const problems = watchConsole(page);
    await page.emulateMedia({ colorScheme: "light" });
    await signIn(page);
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    // Again once lazy content such as a Markdown card has loaded: its
    // headings must not add a second h1.
    await expect(page.locator("h1")).toHaveCount(1);
    await expectTheme(page, "light");
    await expectNoHorizontalOverflow(page);
    await expectAccessible(page);
    await expectGlossaryTerms(page);
    expect(problems).toEqual([]);
  });

  test(`${path} passes axe in the dark theme`, async ({ page }) => {
    const problems = watchConsole(page);
    await page.emulateMedia({ colorScheme: "dark" });
    await signIn(page);
    await page.goto(path);
    // Wait for the page to render before settled(): with no loading
    // placeholder on the page yet, settled() passes at once and axe would
    // check an empty document.
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    await expect(page.locator("h1")).toHaveCount(1);
    await expectTheme(page, "dark");
    await expectAccessible(page);
    expect(problems).toEqual([]);
  });
}

for (const path of signedOut) {
  for (const scheme of schemes) {
    const name =
      scheme === "light"
        ? `${path} renders, fits the width and is accessible when signed out`
        : `${path} passes axe in the dark theme when signed out`;
    test(name, async ({ page }) => {
      const problems = watchConsole(page);
      const failedLoads = watchFailedLoads(page);
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto(path);
      await expect(page.locator("h1")).toHaveCount(1);
      await settled(page);
      // Again once lazy content such as a Markdown card has loaded: its
      // headings must not add a second h1.
      await expect(page.locator("h1")).toHaveCount(1);
      await expectTheme(page, scheme);
      await expectNoHorizontalOverflow(page);
      await expectAccessible(page);
      await expectGlossaryTerms(page);
      expect(problems).toEqual([]);
      // No request fails before sign-in, so the console stays clean.
      expect(failedLoads).toEqual([]);
    });
  }
}

// While anonymous reads are off (the mock's Instance, like a default one),
// profiles answer 401 to signed-out visitors (decided 2026-10-18). The
// page asks them to sign in and sends no other request that would fail.
for (const scheme of schemes) {
  test(`a profile asks signed-out visitors to sign in (${scheme})`, async ({ page }) => {
    const problems = watchConsole(page);
    const failedLoads = watchFailedLoads(page);
    const api: string[] = [];
    page.on("request", (r) => {
      const path = new URL(r.url()).pathname;
      if (path.startsWith("/api/hangar/v1/") && path !== "/api/hangar/v1/me") api.push(path);
    });
    await page.emulateMedia({ colorScheme: scheme });
    await page.goto("/ml-team?tab=members");
    await expect(page.locator("h1")).toHaveText("Sign in to see this profile");
    await expect(page.locator("#main").getByRole("link", { name: "Sign in" })).toHaveAttribute(
      "href",
      `/auth/login?redirect=${encodeURIComponent("/ml-team?tab=members")}`,
    );
    await settled(page);
    await expectTheme(page, scheme);
    await expectNoHorizontalOverflow(page);
    await expectAccessible(page);
    expect(problems).toEqual([]);
    expect(api).toEqual(["/api/hangar/v1/namespaces/ml-team"]);
    expect(failedLoads.filter((f) => !f.includes("/namespaces/ml-team"))).toEqual([]);
  });
}

test("a wide Markdown table scrolls in a region keyboard users can focus", async ({ page }) => {
  const csrf = await signIn(page);
  const columns = Array.from({ length: 16 }, (_, i) => `measurement_column_${i}`);
  const markdown = [
    "# Results",
    "",
    `| ${columns.join(" | ")} |`,
    `|${columns.map(() => "---").join("|")}|`,
    `| ${columns.map((_, i) => `${i}.0`).join(" | ")} |`,
    "",
  ].join("\n");
  const path = `wide-table-${unique()}.md`;
  const commit = await callApi(page, csrf, "POST", "/repos/models/ada/bert-tiny-finetune/commits", {
    summary: `Add ${path}`,
    operations: [{ op: "add", path, content_base64: Buffer.from(markdown).toString("base64") }],
  });
  expect(commit.status()).toBe(201);
  await page.goto(`/ada/bert-tiny-finetune/blob/main/${path}`);
  const region = page.getByRole("region", { name: /^Table: measurement_column_0/ });
  await expect(region.getByRole("table")).toBeVisible();
  // The table is wider than the region at every width, so the region scrolls.
  expect(await region.evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(true);
  await settled(page);
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
});

test("the main navigation works on every width", async ({ page, isMobile }) => {
  await signIn(page);
  await page.goto("/models");
  const go = async (name: string) => {
    if (isMobile) await page.getByRole("button", { name: "Open navigation" }).click();
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name }).click();
    await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
  };
  await go("Datasets");
  await expect(page).toHaveURL(/\/datasets$/);
  await go("Models");
  await expect(page).toHaveURL(/\/models$/);
  await go("Discover");
  if (isMobile) await page.getByRole("button", { name: "Open navigation" }).click();
  await expect(
    page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Arsenal" }),
  ).toHaveCount(0);
});

// Taxiway teal, decided by James on 2026-10-16 (docs/build/decisions.md).
const accents = {
  light: { accent: "#0b6e64", fill: "#0b7a6e", ink: "#ffffff", soft: "#dff3ef" },
  dark: { accent: "#5fd4c2", fill: "#2bb3a0", ink: "#021a17", soft: "#12332e" },
};

for (const scheme of ["light", "dark"] as const) {
  test(`the accent is Taxiway teal in the ${scheme} theme`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await signIn(page);
    await page.goto("/models");
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    const tokens = await page.evaluate(() => {
      const css = getComputedStyle(document.documentElement);
      // The build may shorten #ffffff to #fff.
      const v = (n: string) =>
        css
          .getPropertyValue(n)
          .trim()
          .toLowerCase()
          .replace(/^#([0-9a-f])([0-9a-f])([0-9a-f])$/, "#$1$1$2$2$3$3");
      return {
        accent: v("--hg-accent"),
        fill: v("--hg-accent-fill"),
        ink: v("--hg-accent-ink"),
        soft: v("--hg-accent-soft"),
      };
    });
    expect(tokens).toEqual(accents[scheme]);
    // The logo's bay line and the primary button use the filled accent.
    const toRgb = (hex: string) =>
      `rgb(${[1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16)).join(", ")})`;
    const bay = page.getByRole("link", { name: "Hangar home" }).locator("svg rect").first();
    expect(await bay.evaluate((el) => getComputedStyle(el).fill)).toBe(toRgb(accents[scheme].fill));
    const discover = page.getByRole("main").getByRole("link", { name: "Discover" });
    expect(await discover.evaluate((el) => getComputedStyle(el).backgroundColor)).toBe(
      toRgb(accents[scheme].fill),
    );
    expect(await discover.evaluate((el) => getComputedStyle(el).color)).toBe(
      toRgb(accents[scheme].ink),
    );
    await expectAccessible(page);
  });
}

test("cards in a grid row are the same height, with the stats at the bottom", async ({
  page,
}, info) => {
  test.skip(info.project.name === "phone", "Phones show one card per row.");
  await signIn(page);
  // By name: the GGUF model with five quants and Mirror badges shares a row
  // with models that have one or two badges.
  await page.goto("/models?sort=name");
  await settled(page);
  const cards = page.getByRole("list", { name: "Models" }).locator("article");
  await expect(cards.first()).toBeVisible();
  const boxes = await cards.evaluateAll((els) =>
    els.map((el) => {
      const r = el.getBoundingClientRect();
      const stats = el.querySelector("dl")?.getBoundingClientRect();
      return { top: r.top, height: r.height, bottom: r.bottom, statsBottom: stats?.bottom ?? 0 };
    }),
  );
  const rows = new Map<number, typeof boxes>();
  for (const b of boxes) rows.set(Math.round(b.top), [...(rows.get(Math.round(b.top)) ?? []), b]);
  const full = [...rows.values()].filter((r) => r.length > 1);
  expect(full.length).toBeGreaterThan(0);
  for (const row of full) {
    const heights = row.map((b) => Math.round(b.height));
    expect(new Set(heights).size, `card heights in a row: ${heights.join(", ")}`).toBe(1);
    // The stats row sits at the bottom of every card, so the rows line up.
    const gaps = row.map((b) => Math.round(b.bottom - b.statsBottom));
    expect(new Set(gaps).size, `space under the stats: ${gaps.join(", ")}`).toBe(1);
  }
  // Quant chips past the cap fold into a +N chip that names the rest.
  await expect(
    cards.getByRole("img", { name: /^\d+ more quants?: Q\w+(, Q\w+)*$/ }).first(),
  ).toBeVisible();
});

// Dialogs and panels that open over a page, which must fit the width too.
test("the upload panel with staged files passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/ada/bert-tiny-finetune/tree/main");
  await settled(page);
  await page.getByRole("button", { name: "Upload files" }).click();
  await chooseFiles(page.getByLabel("Choose files"), {
    name: "notes.txt",
    buffer: Buffer.from("hello"),
  });
  await expect(page.getByRole("list", { name: "Files to upload" })).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
});

test("the Use this model dialog passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/TinyLlama/TinyLlama-1.1B-Chat-GGUF");
  await settled(page);
  await page.getByRole("button", { name: "Use this model" }).click();
  await expect(page.getByRole("dialog", { name: "Use this model" })).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
});

test("the search palette passes axe while results load and once they arrive", async ({ page }) => {
  await signIn(page);
  await page.goto("/models");
  await settled(page);
  const dialog = await searchWithKeyboard(page, "imd");
  const status = dialog.getByRole("status");
  const imdb = dialog.getByRole("option", { name: /datasets\/ada\/imdb-mini/ });
  // The results for "imd", once the debounced query has run.
  await expect(imdb).toBeVisible();
  await expect(status).toHaveText("");
  // Hold the next query's results, so axe sees the palette while they load:
  // the listbox must still hold only groups of options.
  let release = () => {};
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(
    (url) => url.pathname === "/api/hangar/v1/search" && url.searchParams.get("q") === "imdb",
    async (route) => {
      await held;
      await route.continue();
    },
  );
  await page.keyboard.type("b");
  await expect(status).toHaveText("Searching");
  // The last results stay while the next ones load.
  await expect(imdb).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
  release();
  await expect(status).toHaveText("");
  await expect(imdb).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
});

test("the dataset card editor passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/datasets/ada/imdb-mini");
  await settled(page);
  await page.getByRole("button", { name: "Edit card" }).click();
  await expect(page.getByLabel("Pretty name")).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
});
