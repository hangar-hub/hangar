import { expect, test } from "@playwright/test";
import { expectAccessible, settled, signIn, watchConsole } from "./helpers";

// Every page in 09-web-ui.md renders, has one h1, and passes axe at phone,
// tablet and desktop widths.
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
  "/ada/bert-tiny-finetune/discussions",
  "/ada/bert-tiny-finetune/discussions/2",
  "/ada/bert-tiny-finetune/settings",
  "/TinyLlama/TinyLlama-1.1B-Chat-GGUF/settings",
  "/datasets/ada/imdb-mini",
  "/datasets/ada/imdb-mini/viewer/default/train",
  "/datasets/ada/cifar-mini/viewer/default/train",
  "/datasets/ml-team/speech-commands-mini/viewer",
  "/new",
  "/new-dataset",
  "/ada",
  "/ml-team?tab=members",
  "/collections",
  "/collections/ada/small-and-fast",
  "/settings/profile",
  "/settings/tokens",
  "/settings/ollama-keys",
  "/settings/upstream-tokens",
  "/settings/sessions",
  "/settings/organisations",
  "/settings/device?code=ABCD-1234",
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

test("dark theme passes axe too", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await signIn(page);
  await page.goto("/ada/bert-tiny-finetune");
  await settled(page);
  await expect(page.locator("html")).toHaveClass(/dark/);
  await expectAccessible(page);
});
