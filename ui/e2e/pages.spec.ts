import { createHash } from "node:crypto";
import { expect, test } from "@playwright/test";
import { expectAccessible, settled, signIn, watchConsole } from "./helpers";

// Every page in 09-web-ui.md renders, has one h1, and passes every axe rule
// at phone, tablet and desktop widths.

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
  "/admin/organisations?q=ml",
  "/admin/upstreams",
  "/admin/pulls",
  "/admin/storage",
  "/admin/jobs",
  "/admin/webhooks",
  "/admin/audit",
  "/admin/settings",
  "/no-such-namespace",
];

const signedOut = [
  "/",
  "/auth/login",
  "/auth/invite/abc123",
  "/auth/reset/abc123",
  "/auth/login?error=oidc_denied",
];

for (const path of signedIn) {
  test(`${path} renders and is accessible`, async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    await expectAccessible(page);
    expect(problems).toEqual([]);
  });
}

for (const path of signedOut) {
  test(`${path} renders and is accessible when signed out`, async ({ page }) => {
    const problems = watchConsole(page);
    await page.goto(path);
    await expect(page.locator("h1")).toHaveCount(1);
    await settled(page);
    await expectAccessible(page);
    expect(problems).toEqual([]);
  });
}

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
    await settled(page);
    await expect(page.locator("html")).toHaveClass(/dark/);
    await expectAccessible(page);
  });
}

// Dialogs and panels that open over a page.
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
  await expectAccessible(page);
});

test("the Use this model dialog passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/TinyLlama/TinyLlama-1.1B-Chat-GGUF");
  await settled(page);
  await page.getByRole("button", { name: "Use this model" }).click();
  await expect(page.getByRole("dialog", { name: "Use this model" })).toBeVisible();
  await expectAccessible(page);
});

test("the search palette passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/arsenal");
  await settled(page);
  const dialog = page.getByRole("dialog", { name: "Search" });
  await expect(async () => {
    await page.keyboard.press("/");
    await expect(dialog).toBeVisible({ timeout: 1000 });
  }).toPass();
  await dialog.getByRole("combobox", { name: "Search Hangar" }).fill("imdb");
  await expect(dialog.getByRole("option").first()).toBeVisible();
  await expectAccessible(page);
});

test("the dataset card editor passes axe", async ({ page }) => {
  await signIn(page);
  await page.goto("/datasets/ada/imdb-mini");
  await settled(page);
  await page.getByRole("button", { name: "Edit card" }).click();
  await expect(page.getByLabel("Pretty name")).toBeVisible();
  await expectAccessible(page);
});
