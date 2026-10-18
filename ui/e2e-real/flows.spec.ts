import { expect, type Page, test } from "@playwright/test";
import {
  chooseFiles,
  commitResponse,
  expectAccessible,
  parseBytes,
  pullRequestDiffResponse,
  settled,
  unique,
  watchConsole,
} from "../e2e/helpers";
import {
  allowForGitWrites,
  api,
  env,
  ownRepo,
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
  await expect(page.getByRole("heading", { level: 1, name: "Models" })).toBeVisible();
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
  allowForGitWrites(1);
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto(`/${modelRepo}/tree/main`);
  const name = `notes-${Date.now()}.txt`;
  await page.getByRole("button", { name: "Upload files" }).click();
  await chooseFiles(page.getByLabel("Choose files"), {
    name,
    buffer: Buffer.from("hello from the browser"),
  });
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
  allowForGitWrites(1);
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto(`/${modelRepo}/tree/main`);
  const name = `proposal-${Date.now()}.md`;
  await page.getByRole("button", { name: "Upload files" }).click();
  await chooseFiles(page.getByLabel("Choose files"), {
    name,
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

// Models and Datasets are separate sections with optional links between them
// (docs/build/decisions.md, 2026-10-16).
test("the Models and Datasets lists keep to their kind, and old Arsenal links still work", async ({
  page,
}) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto("/arsenal?view=list");
  await expect(page).toHaveURL(/\/models\?view=list$/);
  const models = page.getByRole("list", { name: "Models" });
  await expect(models.getByRole("link", { name: modelRepo })).toBeVisible();
  await expect(models.getByRole("link", { name: /^datasets\// })).toHaveCount(0);
  await page.goto("/arsenal?type=datasets");
  await expect(page).toHaveURL(/\/datasets$/);
  const datasets = page.getByRole("list", { name: "Datasets" });
  await expect(datasets.getByRole("link", { name: `datasets/${datasetRepo}` })).toBeVisible();
  await expect(datasets.getByRole("link", { name: modelRepo })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "New dataset" })).toHaveAttribute(
    "href",
    "/new-dataset",
  );
  expect(problems).toEqual([]);
});

test.describe(() => {
  let model = "";
  let dataset = "";
  let samples = "";
  test.beforeAll(async () => {
    allowForGitWrites(6);
    const id = unique();
    // Repos of their own: the picker commits with the head it read as
    // parent_commit, and the dataset's Used by must list only this model.
    model = await ownRepo("model", `uses-data-${id}`, "Add the card", {
      "README.md": "---\nlicense: mit\n---\n\n# Classifier\n\nSorts reviews.\n",
    });
    dataset = await ownRepo("dataset", `used-data-${id}`, "Add the data", {
      "data/train.csv": "text,label\ngood,1\nbad,0\n",
    });
    // Files the Dataset Viewer does not read, as sample data for another
    // project holds.
    samples = await ownRepo("dataset", `samples-${id}`, "Add sample inputs", {
      "README.md": "# Samples\n\nInputs for another project's tests.\n",
      "fixtures/firmware.bin": "not a data file\n",
    });
  });

  test("links a model to the datasets it uses, and the dataset lists it under Used by", async ({
    page,
  }) => {
    allowForGitWrites(1);
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto(`/${model}/settings`);
    const section = page.getByRole("region", { name: "Datasets used" });
    await section.getByLabel("Find a dataset").fill(dataset.split("/")[1] ?? "");
    await section.getByRole("button", { name: `Add datasets/${dataset}` }).click();
    await settled(page);
    await expectAccessible(page);
    const committed = commitResponse(page, `models/${model}`);
    await section.getByRole("button", { name: "Commit to the Model card" }).click();
    expect((await committed).status()).toBe(201);
    await expect(
      page.getByRole("status").filter({ hasText: "Datasets used saved." }),
    ).toBeVisible();
    const raw = await page.request.get(`/${model}/resolve/main/README.md`);
    expect(await raw.text()).toBe(
      `---\nlicense: mit\ndatasets:\n  - ${dataset}\n---\n\n# Classifier\n\nSorts reviews.\n`,
    );
    await page.goto(`/datasets/${dataset}`);
    const usedBy = page
      .getByRole("complementary", { name: "About this Repo" })
      .getByRole("region", { name: "Used by" });
    await expect(usedBy.getByRole("link", { name: model })).toBeVisible();
    await page.goto(`/${model}`);
    await expect(
      page
        .getByRole("complementary", { name: "About this Repo" })
        .getByRole("region", { name: "Datasets used" })
        .getByRole("link", { name: dataset }),
    ).toBeVisible();
    expect(problems).toEqual([]);
  });

  test("a dataset of files the Viewer cannot read explains itself", async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto(`/datasets/${samples}/viewer`);
    await expect(page.getByText("Nothing to preview")).toBeVisible();
    await expect(page.getByText("This dataset has none it can read")).toBeVisible();
    await expect(page.getByRole("button", { name: "Try again" })).toHaveCount(0);
    await page.getByRole("button", { name: "Use this dataset" }).click();
    const dialog = page.getByRole("dialog", { name: "Use this dataset" });
    await expect(dialog.getByRole("tab")).toHaveText(["hf CLI", "huggingface_hub", "git"]);
    await settled(page);
    await expectAccessible(page);
    await page.keyboard.press("Escape");
    await page.getByRole("link", { name: "Browse the files" }).click();
    await expect(page.getByRole("link", { name: "fixtures" })).toBeVisible();
    expect(problems).toEqual([]);
  });
});

// The card editor and file editor flows each start from a Repo of their own,
// made in a hook so its two git writes do not use up the flow's time.
test.describe(() => {
  let id = "";
  test.beforeAll(async () => {
    allowForGitWrites(2);
    // A dataset of its own, so no other flow or width commits to it meanwhile.
    id = await ownRepo("dataset", `card-${unique()}`, "Add the card", {
      "README.md":
        "---\nlicense: cc-by-4.0\nsize_categories:\n- n<1K\n---\n# Animals\n\nLegs per animal.\n",
    });
  });

  test("edits a dataset card with the form", async ({ page }) => {
    allowForGitWrites(1);
    const problems = watchConsole(page);
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
    expect(problems).toEqual([]);
  });
});

test.describe(() => {
  let id = "";
  test.beforeAll(async () => {
    allowForGitWrites(2);
    // A Repo of its own: editing sends the Commit the page opened as
    // parent_commit, so a Commit to the same branch by another flow would
    // rightly be refused with 412.
    id = await ownRepo("model", `edit-${unique()}`, "Add the tokenizer config", {
      "tokenizer_config.json": '{\n  "do_lower_case": true\n}\n',
    });
  });

  test("edits a text file in the browser", async ({ page }) => {
    allowForGitWrites(1);
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
    await expect(page.getByRole("region", { name: "Code" })).toContainText(
      '"do_lower_case": false',
    );
  });
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

/**
 * Clicks Change password and returns the answer. A wrong current password
 * counts towards the sign-in throttle for this host (docs/spec/05-auth.md),
 * so while the Instance locks it the form must say how long to wait; the
 * flow waits that long and submits again, as a person would.
 */
async function submitPasswordChange(page: Page): Promise<number> {
  for (let tries = 1; ; tries++) {
    const [res] = await Promise.all([
      page.waitForResponse(
        (r) =>
          r.request().method() === "POST" && new URL(r.url()).pathname === `${api}/me/password`,
      ),
      page.getByRole("button", { name: "Change password" }).click(),
    ]);
    const lock = signInLock(res);
    if (lock === undefined || tries === 3) return res.status();
    await expect(page.getByRole("alert")).toHaveText(
      /^Too many failed sign-in attempts\. Try again in [\dhms.]+\.$/,
    );
    await waitOutLock(lock);
  }
}

test("changes the display name, email and password on the Profile page", async ({ page }) => {
  const problems = watchConsole(page);
  const admin = await session(env("HANGAR_URL"), "hangar-admin", adminPassword);
  // Users of the flow's own, so other flows keep their passwords.
  const name = `kit${unique()}`;
  const other = `ivy${unique()}`;
  const first = "a-long-enough-password";
  const second = "an-even-longer-passphrase";
  let elsewhere: Awaited<ReturnType<typeof session>> | undefined;
  const ids: Record<string, string> = {};
  try {
    for (const [username, email] of [
      [name, `${name}@example.com`],
      [other, `${other}@example.com`],
    ] as const) {
      const made = await admin.call("POST", "/admin/users", { username, password: first, email });
      expect(made.status(), await made.text()).toBe(201);
      ids[username] = (await made.json()).id as string;
    }
    // Another browser signed in as the same user, which the change signs out.
    elsewhere = await session(env("HANGAR_URL"), name, first);
    expect((await elsewhere.call("GET", "/me/sessions")).status()).toBe(200);

    await signIn(page, name, first);
    await page.goto("/settings/profile");
    await expect(page.getByText(/ask an admin/i)).toHaveCount(0);
    await page.getByLabel("Display name").fill("Kit Marlowe");
    await page.getByRole("textbox", { name: "Email" }).fill(`${other}@example.com`);
    await page.getByRole("button", { name: "Save profile" }).click();
    await expect(page.getByRole("alert")).toHaveText(
      "Already exists: the email address is already used.",
    );
    await page.getByRole("textbox", { name: "Email" }).fill(`${name}@home.example`);
    await page.getByRole("button", { name: "Save profile" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Profile saved." })).toBeVisible();
    await page.reload();
    await expect(page.getByLabel("Display name")).toHaveValue("Kit Marlowe");
    // The new address waits for the confirmation link an admin sends it.
    await expect(page.getByRole("textbox", { name: "Email" })).toHaveValue(`${name}@example.com`);
    await expect(
      page.getByRole("status").filter({ hasText: "waits for confirmation" }),
    ).toContainText(`${name}@home.example`);
    const link = await admin.call("POST", `/admin/users/${ids[name]}/email-link`, {
      email: `${name}@home.example`,
    });
    expect(link.status(), await link.text()).toBe(201);
    expect((await link.json()).email).toBe(`${name}@home.example`);
    const confirmPath = new URL((await link.json()).url as string).pathname;
    await page.goto(confirmPath);
    await page.getByRole("button", { name: "Confirm email" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Email confirmed." })).toBeVisible();
    await page.goto(confirmPath);
    await page.getByRole("button", { name: "Confirm email" }).click();
    await expect(page.getByRole("alert")).toContainText("invalid, used or expired");
    await page.goto("/settings/profile");
    await expect(page.getByRole("textbox", { name: "Email" })).toHaveValue(`${name}@home.example`);

    const current = page.getByLabel("Current password");
    await current.fill("not-my-password-at-all");
    await page.getByLabel("New password", { exact: true }).fill(second);
    await page.getByLabel("Confirm new password").fill(second);
    expect(await submitPasswordChange(page)).toBe(403);
    await expect(page.getByText("The current password is wrong.")).toBeVisible();
    await current.fill(first);
    await page.getByLabel("New password", { exact: true }).fill("password1");
    await page.getByLabel("Confirm new password").fill("password1");
    expect(await submitPasswordChange(page)).toBe(422);
    await expect(page.getByLabel("New password", { exact: true })).toHaveAttribute(
      "aria-invalid",
      "true",
    );
    await page.getByLabel("New password", { exact: true }).fill(second);
    await page.getByLabel("Confirm new password").fill(second);
    expect(await submitPasswordChange(page)).toBe(204);
    await expect(page.getByRole("status").filter({ hasText: "Password changed." })).toBeVisible();
    await settled(page);
    await expectAccessible(page);

    // The other browser is signed out; this one stays signed in.
    expect((await elsewhere.call("GET", "/me/sessions")).status()).toBe(401);
    await page.goto("/settings/sessions");
    await expect(page.getByText("This browser")).toBeVisible();
    await expect(page.getByRole("button", { name: "Sign out", exact: true })).toHaveCount(0);
    // Sign-in takes the new password.
    await (await session(env("HANGAR_URL"), name, second)).dispose();
  } finally {
    await elsewhere?.dispose();
    await admin.dispose();
  }
  expect(problems).toEqual([]);
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
