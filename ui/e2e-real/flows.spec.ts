import { expect, type Page, test } from "@playwright/test";
import {
  commitResponse,
  expectAccessible,
  parseBytes,
  pullRequestDiffResponse,
  settled,
  unique,
  watchConsole,
} from "../e2e/helpers";
import {
  api,
  env,
  session,
  signIn,
  signInLock,
  upstreamGgufRepo,
  upstreamName,
  user,
  waitForPulledFile,
  waitOutLock,
} from "./helpers";
import { datasetRepo, modelRepo } from "./seed";

// Flows against a real Hangar seeded by seed.ts, so the UI is checked
// against the real backends as well as the mock's contract. They change
// state, so they run one at a time, and each width runs them again against
// the same Instances: a flow makes the Repos it changes, with names unique
// to the run, or puts back what it changes.
test.describe.configure({ mode: "serial" });

const adminPassword = process.env.HANGAR_E2E_ADMIN_PASSWORD ?? "";

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

/**
 * Clicks Sign in on the form. While the Instance locks sign-in after
 * earlier failures from this host (each width of each run makes one), the
 * form must say how long to wait; the flow waits that long and submits
 * again, as a person would.
 */
async function submitSignIn(page: Page) {
  for (let tries = 1; ; tries++) {
    const [res] = await Promise.all([
      page.waitForResponse(
        (r) => r.request().method() === "POST" && new URL(r.url()).pathname === `${api}/auth/login`,
      ),
      page.getByRole("button", { name: "Sign in", exact: true }).click(),
    ]);
    const lock = signInLock(res);
    if (lock === undefined || tries === 3) return;
    await expect(page.getByRole("alert")).toHaveText(
      /^Too many failed sign-in attempts\. Try again in [\dhms.]+\.$/,
    );
    await waitOutLock(lock);
  }
}

test("signs in with a password, shows errors, and signs out", async ({ page }) => {
  const problems = watchConsole(page);
  await page.goto("/auth/login");
  await page.getByLabel("Username").fill(user.username);
  await page.getByLabel("Password").fill("wrong-password");
  await submitSignIn(page);
  await expect(page.getByRole("alert")).toHaveText("Wrong username or password.");
  await page.getByLabel("Password").fill(user.password);
  await submitSignIn(page);
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
  const committed = commitResponse(page, `models/${modelRepo}`);
  await page.getByRole("button", { name: "Upload and commit" }).click();
  expect((await committed).status()).toBe(201);
  await expect(page.getByRole("status").filter({ hasText: "Files committed." })).toBeVisible();
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
    await expect(page.getByRole("list", { name: "Imports the scan found" })).toContainText(
      "os.system",
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

test("creates an Organisation and lists it for admins", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page, "hangar-admin", adminPassword);
  await page.goto("/admin/organisations");
  const name = `org-${Date.now()}`;
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create Organisation" }).click();
  await expect(page.getByRole("status").filter({ hasText: `Created ${name}.` })).toBeVisible();
  await expect(page.getByRole("listitem").filter({ hasText: name })).toContainText("1 members");
  await page.getByLabel("Find an Organisation").fill(name);
  await page.getByLabel("Find an Organisation").press("Enter");
  await expect(page.getByRole("listitem")).toHaveCount(1);
  expect(problems).toEqual([]);
});

test("opens a pull request from an upload and shows its diff", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto(`/${modelRepo}/tree/main`);
  const name = `proposal-${Date.now()}.md`;
  await page.getByRole("button", { name: "Upload files" }).click();
  await page.getByLabel("Choose files").setInputFiles({
    name,
    mimeType: "text/markdown",
    buffer: Buffer.from("# Proposal\n"),
  });
  await page.getByLabel("Open as pull request").check();
  const committed = commitResponse(page, `models/${modelRepo}`);
  await page.getByRole("button", { name: "Upload and open pull request" }).click();
  expect((await committed).status()).toBe(201);
  await expect(page).toHaveURL(new RegExp(`/${modelRepo}/discussions/\\d+$`));
  // The tab loads the diff of each file when it opens.
  const diffed = pullRequestDiffResponse(page, `models/${modelRepo}`);
  await page.getByRole("tab", { name: /Files changed/ }).click();
  await expect(page.getByRole("list", { name: "Changed files" })).toContainText(name);
  expect((await diffed).status()).toBe(200);
  await expect(page.getByRole("region", { name: `Diff of ${name}` })).toContainText("# Proposal");
  expect(problems).toEqual([]);
});

test("Discover a model on the Upstream, pull one quant and open it in the GGUF viewer", async ({
  page,
}) => {
  // The Pull is a Job that copies from the second Instance.
  test.setTimeout(120_000);
  const problems = watchConsole(page);
  // A Repo new to this run, so nothing of it is in the arsenal yet.
  const repo = await upstreamGgufRepo();
  await signIn(page);
  await page.goto("/discover");
  await page.getByRole("combobox", { name: "Upstream" }).selectOption(upstreamName);
  const search = page.getByLabel("Search", { exact: true });
  await search.fill(repo.name);
  await search.press("Enter");
  await page
    .getByRole("list", { name: "Results" })
    .getByRole("listitem")
    .filter({ hasText: repo.id })
    .getByRole("button", { name: repo.id })
    .click();
  const panel = page.getByRole("dialog");
  const q4 = panel.getByRole("checkbox", { name: /Q4_K_M/ });
  const q8 = panel.getByRole("checkbox", { name: /Q8_0/ });
  // Q4_K_M is the recommended quant, so it starts chosen.
  await expect(q4).toBeChecked();
  await expect(q8).not.toBeChecked();
  const toDownload = panel.getByText(/To download:/);
  const q4Only = parseBytes(await toDownload.innerText());
  await q8.check();
  await expect.poll(async () => parseBytes(await toDownload.innerText())).toBeGreaterThan(q4Only);
  const both = parseBytes(await toDownload.innerText());
  await q4.uncheck();
  await expect.poll(async () => parseBytes(await toDownload.innerText())).toBeLessThan(both);
  // The card's "# " heading sits under the dialog's title: still one h1.
  await panel.getByRole("tab", { name: "Model card" }).click();
  await expect(panel.getByRole("heading", { level: 3, name: repo.name })).toBeVisible();
  await expect(page.locator("h1")).toHaveCount(1);
  await settled(page);
  await expectAccessible(page);
  await panel.getByRole("button", { name: "Pull into arsenal" }).click();
  await expect(
    page.getByRole("status").filter({ hasText: `Pull of ${repo.id} started` }),
  ).toBeVisible();

  const ada = await session(env("HANGAR_URL"), user.username, user.password);
  try {
    await waitForPulledFile(ada, repo.id, repo.file.Q8_0);
  } finally {
    await ada.dispose();
  }
  await page.goto(`/${repo.id}`);
  await settled(page);
  // The Mirror's card has its own "# " heading too.
  await expect(page.locator("h1")).toHaveCount(1);
  await page.getByRole("navigation", { name: "Repo" }).getByRole("link", { name: "Files" }).click();
  const files = page.getByRole("table", { name: "Files in the Repo root" });
  await expect(files.getByRole("row").filter({ hasText: repo.file.Q4_K_M })).toContainText(
    "Not in arsenal",
  );
  const pulled = files.getByRole("row").filter({ hasText: repo.file.Q8_0 });
  await expect(pulled).toContainText("LFS");
  await expect(pulled).not.toContainText("Not in arsenal");
  await files.getByRole("link", { name: repo.file.Q8_0 }).click();
  await expect(page.getByText("Quantisation", { exact: true }).locator("..")).toContainText("Q8_0");
  await expect(page.getByRole("heading", { name: "Chat template" })).toBeVisible();
  await expect(page.getByText("Vocabulary", { exact: true }).locator("..")).toContainText("6");
  const tensors = page.getByRole("table", { name: "Tensors in this file" });
  await expect(tensors).toContainText("token_embd.weight");
  await page.getByLabel("Filter tensors by name").fill("attn");
  // The header row and the two attention tensors.
  await expect(tensors.getByRole("row")).toHaveCount(3);
  await settled(page);
  await expectAccessible(page);
  await page.goto(`/${repo.id}/blob/main/${repo.file.Q4_K_M}`);
  await expect(page.getByText("This file is not in the arsenal")).toBeVisible();
  expect(problems).toEqual([]);
});

test("edits a dataset card with the form", async ({ page }) => {
  const problems = watchConsole(page);
  // A dataset of its own, so no other flow or width commits to it meanwhile.
  const id = `${user.username}/card-${unique()}`;
  const ada = await session(env("HANGAR_URL"), user.username, user.password);
  try {
    const created = await ada.call("POST", "/repos", {
      type: "dataset",
      namespace: user.username,
      name: id.split("/")[1],
      visibility: "public",
    });
    expect(created.status(), await created.text()).toBe(201);
    const card =
      "---\nlicense: cc-by-4.0\nsize_categories:\n- n<1K\n---\n# Animals\n\nLegs per animal.\n";
    const commit = await ada.call("POST", `/repos/datasets/${id}/commits`, {
      summary: "Add the card",
      operations: [
        { op: "add", path: "README.md", content_base64: Buffer.from(card).toString("base64") },
      ],
    });
    expect(commit.status(), await commit.text()).toBe(201);

    await signIn(page);
    await page.goto(`/datasets/${id}`);
    await page.getByRole("button", { name: "Edit card" }).click();
    await page.getByLabel("Pretty name").fill("Animals (mini)");
    await page.getByLabel("Languages").fill("en, French");
    await expect(page.getByRole("list", { name: "Problems to fix" })).toContainText(
      "French is not a language code",
    );
    await page.getByLabel("Languages").fill("en, fr");
    await expect(page.getByRole("region", { name: "Code" })).toContainText(
      "pretty_name: Animals (mini)",
    );
    await settled(page);
    await expectAccessible(page);
    const committed = commitResponse(page, `datasets/${id}`);
    await page.getByRole("button", { name: "Commit changes" }).click();
    expect((await committed).status()).toBe(201);
    await expect(page.getByRole("status").filter({ hasText: "Dataset card saved." })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "About this Repo" })).toContainText(
      "Animals (mini)",
    );
    // The card keeps the keys the form does not edit.
    const raw = await page.request.get(`/datasets/${id}/resolve/main/README.md`);
    expect(raw.ok()).toBe(true);
    const text = await raw.text();
    expect(text).toContain("pretty_name: Animals (mini)");
    expect(text).toContain("license: cc-by-4.0");
    expect(text).toContain("n<1K");
    expect(text).toContain("Legs per animal.");
  } finally {
    await ada.dispose();
  }
  expect(problems).toEqual([]);
});

test("edits a text file in the browser", async ({ page }) => {
  // A Repo of its own: editing sends the Commit the page opened as
  // parent_commit, so a Commit to the same branch by another flow would
  // rightly be refused with 412.
  const name = `edit-${unique()}`;
  const id = `${user.username}/${name}`;
  const ada = await session(env("HANGAR_URL"), user.username, user.password);
  try {
    const created = await ada.call("POST", "/repos", {
      type: "model",
      namespace: user.username,
      name,
      visibility: "public",
    });
    expect(created.status(), await created.text()).toBe(201);
    const commit = await ada.call("POST", `/repos/models/${id}/commits`, {
      summary: "Add the tokenizer config",
      operations: [
        {
          op: "add",
          path: "tokenizer_config.json",
          content_base64: Buffer.from('{\n  "do_lower_case": true\n}\n').toString("base64"),
        },
      ],
    });
    expect(commit.status(), await commit.text()).toBe(201);
  } finally {
    await ada.dispose();
  }
  await signIn(page);
  await page.goto(`/${id}/blob/main/tokenizer_config.json`);
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page.getByLabel("Edit tokenizer_config.json").fill('{\n  "do_lower_case": false\n}\n');
  const committed = commitResponse(page, `models/${id}`);
  await page.getByRole("button", { name: "Commit to main" }).click();
  expect((await committed).status()).toBe(201);
  await expect(
    page.getByRole("status").filter({ hasText: "Saved tokenizer_config.json." }),
  ).toBeVisible();
  await page.getByRole("tab", { name: "Source" }).click();
  await expect(page.getByRole("region", { name: "Code" })).toContainText('"do_lower_case": false');
});

test("reorders a Collection with the keyboard", async ({ page }) => {
  // A Collection of its own, so the order it starts with is known.
  const title = `Reorder ${unique()}`;
  const ada = await session(env("HANGAR_URL"), user.username, user.password);
  let slug = "";
  try {
    const made = await ada.call("POST", "/collections", {
      namespace: user.username,
      title,
      visibility: "public",
    });
    expect(made.status(), await made.text()).toBe(201);
    slug = (await made.json()).slug as string;
    for (const [type, item_id] of [
      ["model", modelRepo],
      ["dataset", datasetRepo],
    ]) {
      const added = await ada.call("POST", `/collections/${user.username}/${slug}/items`, {
        type,
        item_id,
      });
      expect(added.ok(), await added.text()).toBe(true);
    }
  } finally {
    await ada.dispose();
  }
  await signIn(page);
  await page.goto(`/collections/${user.username}/${slug}`);
  await page.getByRole("button", { name: "Edit" }).click();
  const items = page.getByRole("list", { name: "Items" }).getByRole("listitem");
  await expect(items.first()).toContainText(modelRepo);
  await page.getByRole("button", { name: `Move ${modelRepo}`, exact: true }).focus();
  // dnd-kit measures between key presses, so give each step a frame or two.
  await page.keyboard.press("Space");
  await page.waitForTimeout(150);
  await page.keyboard.press("ArrowDown");
  await page.waitForTimeout(150);
  await page.keyboard.press("Space");
  await expect(items.first()).toContainText(datasetRepo);
  await page.reload();
  await expect(
    page.getByRole("list", { name: "Items" }).getByRole("listitem").first(),
  ).toContainText(datasetRepo);
});

test("changes Instance settings and puts them back", async ({ page }) => {
  await signIn(page, "hangar-admin", adminPassword);
  await page.goto("/admin/settings");
  const anon = page.getByRole("switch", { name: "Anonymous read" });
  await expect(anon).toBeVisible();
  const before = await anon.isChecked();
  const save = async (on: boolean) => {
    const s = page.getByRole("switch", { name: "Anonymous read" });
    if ((await s.isChecked()) !== on) await s.click();
    await page.getByRole("button", { name: "Save settings" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Settings saved." })).toBeVisible();
    await page.reload();
    await expect(page.getByRole("switch", { name: "Anonymous read" })).toBeChecked({
      checked: on,
    });
  };
  try {
    await save(!before);
  } finally {
    await save(before);
  }
});

test("manages users, Upstreams, storage and the audit log", async ({ page }) => {
  const problems = watchConsole(page);
  const admin = await session(env("HANGAR_URL"), "hangar-admin", adminPassword);
  // A user of its own, whose name no other user's contains.
  const name = `grace${unique()}`;
  let tokenId = "";
  try {
    const made = await admin.call("POST", "/admin/users", {
      username: name,
      password: "a-long-enough-password",
      email: `${name}@example.com`,
    });
    expect(made.status(), await made.text()).toBe(201);
    // A token.created event to find in the audit log.
    const token = await admin.call("POST", "/me/tokens", {
      name: `audit-${unique()}`,
      scope: "read",
    });
    expect(token.status(), await token.text()).toBe(201);
    tokenId = (await token.json()).id as string;

    await signIn(page, "hangar-admin", adminPassword);
    await page.goto("/admin/users");
    await page
      .getByRole("row")
      .filter({ hasText: name })
      .getByRole("button", { name: "Reset link" })
      .click();
    await expect(
      page.getByRole("dialog", { name: `Password reset link for ${name}` }),
    ).toContainText("/auth/reset/");
    await page.getByRole("button", { name: "Done" }).click();
    await page.goto("/admin/upstreams");
    await page
      .getByRole("listitem")
      .filter({ hasText: "Second Instance" })
      .getByRole("button", { name: "Test connection" })
      .click();
    await expect(page.getByRole("status").filter({ hasText: "Connected." })).toBeVisible();
    await page.goto("/admin/storage");
    await expect(page.getByRole("row").filter({ hasText: user.username }).first()).toBeVisible();
    await page.goto("/admin/audit?action=token.created");
    await expect(page.getByRole("table", { name: "Audit events" })).toContainText("token.created");
    await settled(page);
    await expectAccessible(page);
  } finally {
    if (tokenId) expect((await admin.call("DELETE", `/me/tokens/${tokenId}`)).ok()).toBe(true);
    await admin.dispose();
  }
  expect(problems).toEqual([]);
});
