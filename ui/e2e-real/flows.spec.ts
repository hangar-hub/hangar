import { expect, type Page, test } from "@playwright/test";
import { expectAccessible, settled, watchConsole } from "../e2e/helpers";
import { datasetRepo, modelRepo, user } from "./seed";

// Flows against a real Hangar seeded by seed.ts, so the UI is checked
// against the real backends as well as the mock's contract. They change
// state, so they run one at a time.
test.describe.configure({ mode: "serial" });

const adminPassword = process.env.HANGAR_E2E_ADMIN_PASSWORD ?? "";

async function signIn(page: Page, username = user.username, password = user.password) {
  // Start a new session: sign-in with a session cookie needs its CSRF token.
  await page.context().clearCookies();
  const res = await page.request.post("/api/hangar/v1/auth/login", {
    data: { username, password },
  });
  expect(res.ok(), await res.text()).toBe(true);
}

async function setBlockUnsafe(page: Page, on: boolean) {
  await signIn(page, "hangar-admin", adminPassword);
  await page.goto("/admin/settings");
  const block = page.getByRole("switch", { name: "Block downloads of unsafe files" });
  if ((await block.isChecked()) !== on) await block.click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Settings saved." })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("switch", { name: "Block downloads of unsafe files" })).toBeChecked({
    checked: on,
  });
}

test("signs in with a password, shows errors, and signs out", async ({ page }) => {
  const problems = watchConsole(page);
  await page.goto("/auth/login");
  await page.getByLabel("Username").fill(user.username);
  await page.getByLabel("Password").fill("wrong-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveText("Wrong username or password.");
  await page.getByLabel("Password").fill(user.password);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Arsenal" })).toBeVisible();
  await expect(page.getByRole("link", { name: modelRepo })).toBeVisible();
  await page.getByRole("button", { name: `Account menu for ${user.username}` }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page.getByRole("link", { name: "Sign in" }).first()).toBeVisible();
  expect(problems).toEqual([]);
});

test("browses files and reads a safetensors header", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto(`/${modelRepo}/tree/main`);
  const files = page.getByRole("table", { name: "Files in the Repo root" });
  await expect(files.getByRole("row").filter({ hasText: "model.safetensors" })).toContainText(
    "LFS",
  );
  await files.getByRole("link", { name: "model.safetensors" }).click();
  await expect(page.getByRole("table", { name: "Tensors in this file" })).toContainText(
    "linear.weight",
  );
  await settled(page);
  await expectAccessible(page);
  expect(problems).toEqual([]);
});

test("creates a Token that is shown once, then revokes it", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto("/settings/tokens");
  await page.getByRole("button", { name: "Create Token" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a Token" });
  const name = `e2e-${Date.now()}`;
  await dialog.getByLabel("Name", { exact: true }).fill(name);
  await dialog.getByRole("radio", { name: /^read/ }).check();
  await dialog.getByRole("button", { name: "Create Token" }).click();
  const shown = page.getByRole("dialog", { name: "Copy your new Token" });
  const secret = (await shown.getByTestId("token-secret").textContent()) ?? "";
  expect(secret).toMatch(/^hgr_/);
  // The Token works against the Hub API.
  const whoami = await page.request.get("/api/whoami-v2", {
    headers: { Authorization: `Bearer ${secret}` },
  });
  expect((await whoami.json()).name).toBe(user.username);
  await shown.getByRole("button", { name: "I've copied it" }).click();
  await page.reload();
  const row = page.getByRole("row").filter({ hasText: name });
  await expect(row).toContainText("read");
  expect(await page.content()).not.toContain(secret);
  await row.getByRole("button", { name: "Revoke" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke Token" }).click();
  await expect(page.getByRole("row").filter({ hasText: name })).toHaveCount(0);
  const revoked = await page.request.get("/api/whoami-v2", {
    headers: { Authorization: `Bearer ${secret}` },
  });
  expect(revoked.status()).toBe(401);
  expect(problems).toEqual([]);
});

test("uploads a file in the browser and commits it", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto(`/${modelRepo}/tree/main`);
  const name = `notes-${Date.now()}.txt`;
  await page.getByRole("button", { name: "Upload files" }).click();
  await page
    .getByLabel("Choose files")
    .setInputFiles([
      { name, mimeType: "text/plain", buffer: Buffer.from("hello from the browser") },
    ]);
  await page.getByLabel("Commit message").fill(`Add ${name}`);
  await page.getByRole("button", { name: "Upload and commit" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Files committed." })).toBeVisible({
    timeout: 20_000,
  });
  const files = page.getByRole("table", { name: "Files in the Repo root" });
  await files.getByRole("link", { name }).first().click();
  await expect(page.getByRole("region", { name: "Code" })).toContainText("hello from the browser");
  expect(problems).toEqual([]);
});

test("blocks an unsafe pickle until the user accepts the risk", async ({ page }) => {
  await setBlockUnsafe(page, true);
  try {
    await signIn(page);
    await page.goto(`/${modelRepo}/blob/main/evil.pkl`);
    await expect(page.getByRole("heading", { name: /Pickle scan/ })).toContainText(
      "Unsafe imports",
    );
    const plain = await page.request.get(`/${modelRepo}/resolve/main/evil.pkl`);
    expect(plain.status()).toBe(403);
    await page.getByRole("button", { name: "Download", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Download evil.pkl?" });
    await expect(dialog).toContainText("can run code");
    const download = page.waitForEvent("download");
    await dialog.getByRole("button", { name: "Download anyway" }).click();
    expect((await download).suggestedFilename()).toBe("evil.pkl");
  } finally {
    await setBlockUnsafe(page, false);
  }
});

test("pages and searches rows in the Dataset Viewer", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto(`/datasets/${datasetRepo}`);
  await page
    .getByRole("navigation", { name: "Repo" })
    .getByRole("link", { name: "Viewer" })
    .click();
  await expect(page.getByText("Rows 1 to 40 of 40")).toBeVisible();
  await page.getByLabel("Search the rows").fill("spider");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  await expect(page.getByRole("table", { name: "Rows" }).getByRole("row").nth(1)).toContainText(
    "spider",
  );
  expect(problems).toEqual([]);
});
