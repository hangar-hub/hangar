import { createHash } from "node:crypto";
import { expect, test } from "@playwright/test";
import {
  callApi,
  expectAccessible,
  expectGlossaryTerms,
  expectNoHorizontalOverflow,
  searchWithKeyboard,
  settled,
  signIn,
  unique,
  watchConsole,
} from "./helpers";

// Every page in 09-web-ui.md renders, has one h1, passes every axe rule,
// avoids the glossary's "Avoid" terms and fits the viewport without scrolling
// sideways, at phone (390 px), tablet and desktop widths.

/** A Commit SHA in the mock's fixture data (ui/mock/data.ts makeCommits). */
const commitSha = (repo: string, title: string) =>
  createHash("sha1").update(`${repo}:${title}`).digest("hex");

const signedIn = [
  "/",
  "/arsenal",
  "/arsenal?type=datasets&view=list",
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
  "/auth/login?error=oidc_denied",
];

for (const path of signedIn) {
  test(`${path} renders, fits the width and is accessible`, async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    // Again once lazy content such as a Markdown card has loaded: its
    // headings must not add a second h1.
    await expect(page.locator("h1")).toHaveCount(1);
    await expectNoHorizontalOverflow(page);
    await expectAccessible(page);
    await expectGlossaryTerms(page);
    expect(problems).toEqual([]);
  });
}

for (const path of signedOut) {
  test(`${path} renders, fits the width and is accessible when signed out`, async ({ page }) => {
    const problems = watchConsole(page);
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    // Again once lazy content such as a Markdown card has loaded: its
    // headings must not add a second h1.
    await expect(page.locator("h1")).toHaveCount(1);
    await expectNoHorizontalOverflow(page);
    await expectAccessible(page);
    await expectGlossaryTerms(page);
    expect(problems).toEqual([]);
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
  await page.goto("/arsenal");
  if (isMobile) {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await page
    .getByRole("navigation", { name: "Main" })
    .getByRole("link", { name: "Discover" })
    .click();
  await expect(page.getByRole("heading", { level: 1, name: "Discover" })).toBeVisible();
});

// The dark theme on a page of each kind.
const darkPages = [
  "/arsenal",
  "/ada/bert-tiny-finetune",
  "/ada/bert-tiny-finetune/blob/main/config.json",
  "/datasets/ada/imdb-mini/viewer/default/train",
  "/discover?repo=TinyLlama/TinyLlama-1.1B-Chat-GGUF",
  "/settings/tokens",
  "/admin/jobs",
];

for (const path of darkPages) {
  test(`${path} passes axe in the dark theme`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: "dark" });
    await signIn(page);
    await page.goto(path);
    // Wait for the page to render before settled(): with no loading
    // placeholder on the page yet, settled() passes at once and axe would
    // check an empty document.
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    await expect(page.locator("h1")).toHaveCount(1);
    await expect(page.locator("html")).toHaveClass(/dark/);
    await expectAccessible(page);
  });
}

// Dialogs and panels that open over a page, which must fit the width too.
test("the upload panel with staged files passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/ada/bert-tiny-finetune/tree/main");
  await settled(page);
  await page.getByRole("button", { name: "Upload files" }).click();
  await page.getByLabel("Choose files").setInputFiles({
    name: "notes.txt",
    mimeType: "text/plain",
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
  await page.goto("/arsenal");
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
