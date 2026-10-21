import { expect, type Page, type Route, test } from "@playwright/test";
import {
  callApi,
  chooseFiles,
  commitResponse,
  expectAccessible,
  expectTheme,
  parseBytes,
  searchWithKeyboard,
  seedBulkRepos,
  settled,
  signIn,
  unique,
  watchConsole,
} from "./helpers";

// The flows 15-build-plan.md asks W8 to cover: sign-in, Discover and Pull,
// upload, browsing files and the GGUF viewer, Token creation and admin
// settings; plus the dataset Viewer, card editor, search and Collections.
//
// They run against the mock, where each browser session has its own copy of
// the fixture data, so flows that change it cannot affect each other or the
// other widths. Anything a flow needs beyond the fixture data (invites,
// Tokens, 260 Repos) it makes through the API first. e2e-real/ runs these
// flows against a real Instance, with data it seeds itself.

test.describe("sign-in", () => {
  test("signs in with a password, shows errors, and signs out", async ({ page }) => {
    const problems = watchConsole(page);
    await page.goto("/");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(
      "A self-hosted home for your arsenal of models.",
    );
    await page.getByRole("link", { name: "Sign in" }).first().click();
    await expect(page).toHaveURL(/\/auth\/login/);
    await page.getByLabel("Username").fill("ada");
    await page.getByLabel("Password").fill("wrong-password");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page.getByRole("alert")).toHaveText("Wrong username or password.");
    await page.getByLabel("Password").fill("correct-horse-battery");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Models" })).toBeVisible();
    await page.getByRole("button", { name: "Account menu for ada" }).click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(
      "A self-hosted home for your arsenal of models.",
    );
    expect(problems).toEqual([]);
  });

  test("signs in with an OIDC provider and returns to the page", async ({ page }) => {
    await page.goto("/auth/login?redirect=%2Fcollections");
    await page.getByRole("link", { name: "Sign in with UniFi" }).click();
    await expect(page).toHaveURL(/\/collections$/);
    await expect(page.getByRole("heading", { level: 1, name: "Collections" })).toBeVisible();
  });

  test("sends a user whose session ended to sign in, then back to the page", async ({ page }) => {
    await signIn(page);
    await page.goto("/settings/profile");
    await expect(page.getByRole("heading", { level: 1, name: "Profile" })).toBeVisible();
    // The session ends behind the page's back, as when it expires or is
    // revoked from another browser.
    await page.context().clearCookies();
    await page
      .getByRole("navigation", { name: "Settings" })
      .getByRole("link", { name: "Tokens", exact: true })
      .click();
    await expect(page).toHaveURL(/\/auth\/login\?redirect=%2Fsettings%2Ftokens&ended=1$/);
    await expect(
      page.getByRole("status").filter({ hasText: "Your session has ended." }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: /^Account menu/ })).toHaveCount(0);
    await page.getByLabel("Username").fill("ada");
    await page.getByLabel("Password").fill("correct-horse-battery");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).toHaveURL(/\/settings\/tokens$/);
    await expect(page.getByRole("heading", { level: 1, name: "Tokens" })).toBeVisible();
  });

  test("the header's Sign in link returns to the page", async ({ page }) => {
    await page.goto("/collections?sort=name");
    await page.getByRole("banner").getByRole("link", { name: "Sign in" }).click();
    await expect(page).toHaveURL(/\/auth\/login\?redirect=%2Fcollections%3Fsort%3Dname$/);
    await page.getByLabel("Username").fill("ada");
    await page.getByLabel("Password").fill("correct-horse-battery");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).toHaveURL(/\/collections\?sort=name$/);
  });

  test("explains OIDC errors", async ({ page }) => {
    await page.goto("/auth/login?error=oidc_link_required");
    await expect(page.getByRole("alert")).toContainText("link the provider from Settings");
  });

  test("accepts an invite", async ({ page }) => {
    // Invite codes are random: make one as an admin, then sign out.
    const csrf = await signIn(page);
    const invite = await callApi(page, csrf, "POST", "/admin/invites", {});
    expect(invite.status()).toBe(201);
    const { url } = (await invite.json()) as { url: string };
    expect((await callApi(page, csrf, "POST", "/auth/logout")).ok()).toBe(true);
    await page.goto(new URL(url).pathname);
    await page.getByLabel("Username").fill(`grace-${unique()}`);
    await page.getByLabel("Password", { exact: true }).fill("short");
    await page.getByLabel("Confirm password").fill("short");
    await page.getByRole("button", { name: "Create account" }).click();
    await expect(page.getByText("Use at least 10 characters.")).toBeVisible();
    await page.getByLabel("Password", { exact: true }).fill("a-long-enough-password");
    await page.getByLabel("Confirm password").fill("a-long-enough-password");
    await page.getByRole("button", { name: "Create account" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Models" })).toBeVisible();
  });
});

test("Discover a model and pull one quant into the arsenal", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto("/discover");
  await page.getByLabel("Search", { exact: true }).fill("tinyllama");
  await page.getByLabel("Search", { exact: true }).press("Enter");
  const result = page
    .getByRole("list", { name: "Results" })
    .getByRole("listitem")
    .filter({ hasText: "TinyLlama/TinyLlama-1.1B-Chat-GGUF" });
  await expect(result.getByText("Partly in arsenal")).toBeVisible();
  await result.getByRole("button", { name: "TinyLlama/TinyLlama-1.1B-Chat-GGUF" }).click();
  const panel = page.getByRole("dialog");
  await expect(panel.getByRole("checkbox", { name: /Q4_K_M/ })).toBeChecked();
  // Choosing Q8_0, which is not in the arsenal, adds its size to the download.
  const toDownload = panel.getByText(/To download:/);
  const before = parseBytes(await toDownload.innerText());
  const q8 = panel.getByRole("checkbox", { name: /Q8_0/ });
  const q8Size = parseBytes(await labelOf(panel.page(), q8));
  await q8.check();
  await expect.poll(async () => parseBytes(await toDownload.innerText())).toBeGreaterThan(before);
  // Each size is rounded to one decimal place, so allow for that.
  expect(parseBytes(await toDownload.innerText())).toBeGreaterThanOrEqual(q8Size * 0.95);
  await expectAccessible(page);
  await panel.getByRole("button", { name: "Pull into arsenal" }).click();
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "Pull of TinyLlama/TinyLlama-1.1B-Chat-GGUF started" }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page).not.toHaveURL(/repo=/);
  await page.getByRole("button", { name: /^Jobs, \d+ running$/ }).click();
  const drawer = page.getByRole("dialog", { name: "Jobs" });
  await expect(drawer.getByText("Pull: TinyLlama/TinyLlama-1.1B-Chat-GGUF")).toHaveCount(2);
  await expect(drawer.getByRole("progressbar").first()).toBeVisible();
  expect(problems).toEqual([]);
});

test("Discover a dataset and choose a Parquet-only Pull", async ({ page }) => {
  await signIn(page);
  await page.goto("/discover?type=datasets&repo=stanfordnlp/imdb");
  const panel = page.getByRole("dialog");
  const train = panel.getByRole("checkbox", { name: "train" });
  await train.check();
  await panel.getByRole("checkbox", { name: "Parquet export only" }).check();
  // A Parquet-only Pull downloads the chosen Split's Parquet export, whose
  // size the Split's label shows.
  const parquet = /, (\S+ \S+) Parquet/.exec(await labelOf(page, train))?.[1];
  expect(parquet).toBeTruthy();
  await expect(panel.getByText(/To download:/)).toContainText(`To download: ${parquet} of`);
  await panel.getByRole("tab", { name: "Preview" }).click();
  await expect(panel.getByRole("table", { name: "First rows" })).toContainText(
    "A wonderful little production.",
  );
  await panel.getByRole("button", { name: "Pull into arsenal" }).click();
  await expect(panel.getByRole("button", { name: "Pull started" })).toBeDisabled();
});

test("loads no external images in cards when the Instance turns them off", async ({ page }) => {
  const csrf = await signIn(page);
  const off = await callApi(page, csrf, "PATCH", "/admin/settings", { ui_external_images: false });
  expect(off.ok(), await off.text()).toBe(true);
  const external: string[] = [];
  await page.route(
    (url) => url.host === "example.com",
    (route) => {
      external.push(route.request().url());
      return route.abort();
    },
  );
  await page.goto("/ada/bert-tiny-finetune");
  await expect(
    page.getByText(
      "Image from example.com not loaded: this Instance does not load external images.",
    ),
  ).toBeVisible();
  // Discover shows the Upstream's card, which uses a picture whose source
  // would load its srcset in place of the img.
  await page.goto("/discover?repo=ada/bert-tiny-finetune");
  const panel = page.getByRole("dialog");
  await panel.getByRole("tab", { name: "Model card" }).click();
  await expect(panel.getByRole("heading", { name: "bert-tiny-finetune" })).toBeVisible();
  await expect(panel.getByText("Image from example.com not loaded")).toBeVisible();
  expect(external).toEqual([]);
});

test.describe("upload", () => {
  test("uploads small and large files in the browser and commits them", async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    // Unique names and content, so the upload sends parts and the Commit
    // changes something however often the flow runs against an Instance.
    const id = unique();
    const notes = `notes-${id}.txt`;
    const weights = `weights-${id}.safetensors`;
    const buffer = Buffer.alloc(6 * 1024 * 1024, 7);
    buffer.write(id);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    await page.getByRole("button", { name: "Upload files" }).click();
    await chooseFiles(page.getByLabel("Choose files"), [
      { name: notes, buffer: Buffer.from(`hello from the browser ${id}`) },
      { name: weights, buffer },
    ]);
    const list = page.getByRole("list", { name: "Files to upload" });
    await expect(list.getByRole("listitem")).toHaveCount(2);
    await expect(page.getByText("2 files, 6.0 MiB")).toBeVisible();
    await page.getByLabel("Commit message").fill("Add notes and weights");
    await page.getByRole("button", { name: "Upload and commit" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Files committed." })).toBeVisible({
      timeout: 20_000,
    });
    const files = page.getByRole("table", { name: "Files in the Repo root" });
    await expect(files.getByRole("link", { name: weights })).toBeVisible();
    await expect(files.getByRole("row").filter({ hasText: weights })).toContainText("LFS");
    await files.getByRole("link", { name: notes }).click();
    await expect(page.getByRole("region", { name: "Code" })).toContainText(
      `hello from the browser ${id}`,
    );
    expect(problems).toEqual([]);
  });

  test("resumes an upload after a page reload, sending only the missing parts", async ({
    page,
  }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    // Hangar's parts are at least 16 MiB (repo.PartSize), so 20 MiB is two
    // parts. Unique content keeps a stored copy from a previous run from
    // turning the upload into "exists".
    const buffer = Buffer.alloc(20 * 1024 * 1024, 9);
    buffer.write(unique());
    const weights = { name: "resume.safetensors", buffer };
    // Hold part 2 so the page is reloaded with part 1 stored and part 2 not.
    let held: Route | undefined;
    await page.route("**/uploads/*/parts/2", (route) => {
      held = route;
    });
    const part1 = page.waitForResponse(
      (r) => r.request().method() === "PUT" && /\/uploads\/[^/]+\/parts\/1$/.test(r.url()),
    );
    await page.getByRole("button", { name: "Upload files" }).click();
    await chooseFiles(page.getByLabel("Choose files"), weights);
    await page.getByRole("button", { name: "Upload and commit" }).click();
    const first = await part1;
    expect(first.ok()).toBe(true);
    const uploadId = first.url().split("/uploads/")[1]?.split("/")[0];
    await expect.poll(() => held !== undefined).toBe(true);

    // The page warns before leaving during an upload; leave anyway.
    const dialogs: string[] = [];
    page.on("dialog", (d) => {
      dialogs.push(d.type());
      void d.accept();
    });
    await page.reload();
    await page.unrouteAll({ behavior: "ignoreErrors" });
    expect(dialogs).toEqual(["beforeunload"]);

    // The plan continues the first upload, as W2's does, and the browser sends only the missing part.
    const puts: string[] = [];
    page.on("request", (r) => {
      if (r.method() === "PUT") puts.push(new URL(r.url()).pathname);
    });
    await page.getByRole("button", { name: "Upload files" }).click();
    await chooseFiles(page.getByLabel("Choose files"), weights);
    await page.getByRole("button", { name: "Upload and commit" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Files committed." })).toBeVisible({
      timeout: 20_000,
    });
    expect(puts).toEqual([
      `/api/hangar/v1/repos/models/ada/bert-tiny-finetune/uploads/${uploadId}/parts/2`,
    ]);
    const files = page.getByRole("table", { name: "Files in the Repo root" });
    await expect(
      files.getByRole("link", { name: "resume.safetensors", exact: true }),
    ).toBeVisible();
    expect(problems).toEqual([]);
  });

  test("adds files in the folder on screen, and only on branches", async ({ page }) => {
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    // Open both at the root first: they must follow the user into a folder.
    await page.getByRole("button", { name: "New file" }).click();
    await expect(
      page.getByRole("dialog", { name: "New file" }).getByLabel("File path"),
    ).toHaveValue("");
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "Upload files" }).click();
    await page
      .getByRole("table", { name: "Files in the Repo root" })
      .getByRole("link", { name: "onnx", exact: true })
      .click();
    await expect(page.getByRole("table", { name: "Files in onnx" })).toBeVisible();
    await chooseFiles(page.getByLabel("Choose files"), [
      { name: "notes.txt", buffer: Buffer.from("notes") },
    ]);
    await expect(page.getByLabel("Upload into folder")).toHaveValue("onnx");
    await expect(page.getByRole("list", { name: "Files to upload" })).toContainText(
      "onnx/notes.txt",
    );
    await page.getByRole("button", { name: "New file" }).click();
    await expect(
      page.getByRole("dialog", { name: "New file" }).getByLabel("File path"),
    ).toHaveValue("onnx/");
    await page.keyboard.press("Escape");
    // A tag names a fixed Commit: nothing can be added there.
    await page.goto("/ada/bert-tiny-finetune/tree/v1.0");
    await expect(page.getByRole("table", { name: "Files in the Repo root" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Upload files" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "New file" })).toHaveCount(0);
  });

  test("opens a pull request from an upload", async ({ page }) => {
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    await page.getByRole("button", { name: "Upload files" }).click();
    await chooseFiles(page.getByLabel("Choose files"), {
      name: "extra.md",
      buffer: Buffer.from("# Extra\n"),
    });
    await page.getByLabel("Open as pull request").check();
    const committed = commitResponse(page, "models/ada/bert-tiny-finetune");
    await page.getByRole("button", { name: "Upload and open pull request" }).click();
    expect((await committed).status()).toBe(201);
    // The number is the next free one in the Repo.
    await expect(page).toHaveURL(/\/ada\/bert-tiny-finetune\/discussions\/\d+$/);
    await expect(page.getByRole("heading", { level: 2 })).toContainText("Upload extra.md");
    await page.getByRole("tab", { name: /Files changed/ }).click();
    await expect(page.getByRole("list", { name: "Changed files" })).toContainText("extra.md");
    await expect(page.getByRole("region", { name: "Diff of extra.md" })).toContainText("# Extra");
  });
});

test.describe("browse files", () => {
  test("opens the GGUF viewer without downloading the file", async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto("/TinyLlama/TinyLlama-1.1B-Chat-GGUF/tree/main");
    const files = page.getByRole("table", { name: "Files in the Repo root" });
    await expect(files.getByRole("row").filter({ hasText: "Q8_0" })).toContainText(
      "Not in arsenal",
    );
    await files.getByRole("link", { name: "tinyllama-1.1b-chat.Q4_K_M.gguf" }).click();
    await expect(page.getByText("Quantisation", { exact: true }).locator("..")).toContainText(
      "Q4_K_M",
    );
    await expect(page.getByRole("heading", { name: "Chat template" })).toBeVisible();
    await expect(page.getByText("Vocabulary").locator("..")).toContainText("32,000");
    const tensors = page.getByRole("table", { name: "Tensors in this file" });
    await expect(tensors).toContainText("token_embd.weight");
    await page.getByLabel("Filter tensors by name").fill("attn");
    await expect(tensors.getByRole("row")).toHaveCount(3);
    await page
      .getByRole("navigation", { name: "Breadcrumb" })
      .getByRole("link", { name: "TinyLlama-1.1B-Chat-GGUF" })
      .click();
    await files.getByRole("link", { name: "tinyllama-1.1b-chat.Q8_0.gguf" }).click();
    await expect(page.getByText("This file is not in the arsenal")).toBeVisible();
    expect(problems).toEqual([]);
  });

  test("shows safetensors headers, pickle scans, Commits and diffs", async ({ page }) => {
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/blob/main/model.safetensors");
    await expect(page.getByRole("table", { name: "Tensors in this file" })).toContainText(
      "bert.embeddings.word_embeddings.weight",
    );
    await expect(page.getByText("F32 parameters").locator("..")).toContainText("4M");
    await page.goto("/ada/legacy-checkpoint/blob/main/model.ckpt");
    await expect(page.getByRole("heading", { name: /Pickle scan/ })).toContainText(
      "Unsafe imports",
    );
    const imports = page.getByRole("list", { name: "Imports the scan found" });
    await expect(imports).toContainText("posix.system");
    await expect(imports).not.toContainText("collections.OrderedDict");
    await page.goto("/ada/bert-tiny-finetune/commits/main");
    await page.getByRole("link", { name: "Add safetensors weights and ONNX export" }).click();
    const readme = page
      .getByRole("list", { name: "Changed files" })
      .getByRole("listitem")
      .filter({ hasText: "README.md" });
    await expect(readme.getByRole("region", { name: "Diff" })).toContainText(
      "+| Accuracy | 0.81 |",
    );
    await expect(
      page
        .getByRole("list", { name: "Changed files" })
        .getByRole("listitem")
        .filter({ hasText: "model.safetensors" }),
    ).toContainText("Added");
  });

  test("edits a text file in the browser", async ({ page }) => {
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/blob/main/tokenizer_config.json");
    await page.getByRole("button", { name: "Edit", exact: true }).click();
    await page.getByLabel("Edit tokenizer_config.json").fill('{\n  "do_lower_case": false\n}\n');
    const committed = commitResponse(page, "models/ada/bert-tiny-finetune");
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

  test("refuses an edit when someone else committed first", async ({ page }) => {
    const csrf = await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/blob/main/tokenizer_config.json");
    await page.getByRole("button", { name: "Edit", exact: true }).click();
    await page.getByLabel("Edit tokenizer_config.json").fill('{\n  "mine": true\n}\n');
    const other = await callApi(
      page,
      csrf,
      "POST",
      "/repos/models/ada/bert-tiny-finetune/commits",
      {
        summary: "Someone else's change",
        branch: "main",
        operations: [
          {
            op: "add",
            path: "tokenizer_config.json",
            content_base64: Buffer.from('{\n  "theirs": true\n}\n').toString("base64"),
          },
        ],
      },
    );
    expect(other.status()).toBe(201);
    const committed = commitResponse(page, "models/ada/bert-tiny-finetune");
    await page.getByRole("button", { name: "Commit to main" }).click();
    expect((await committed).status()).toBe(412);
    await expect(page.getByRole("alert")).toContainText(
      "Someone committed to main after you started editing, so Hangar did not save your change.",
    );
    const raw = await page.request.get(
      "/ada/bert-tiny-finetune/resolve/main/tokenizer_config.json",
    );
    expect(await raw.text()).toContain('"theirs": true');
  });

  test("shows the Use this model snippets", async ({ page }) => {
    await signIn(page);
    await page.goto("/ml-team/qwen-coder-remote");
    await page.getByRole("button", { name: "Use this model" }).click();
    const panel = page.getByRole("dialog", { name: "Use this model" });
    await expect(panel.getByRole("region", { name: "Code" }).first()).toContainText(
      "trust_remote_code=True",
    );
    await panel.getByRole("tab", { name: "git" }).click();
    await expect(panel.getByRole("tabpanel")).toContainText("GIT_LFS_SKIP_SMUDGE=1 git clone");
  });
});

test("creates a Token that is shown once, then revokes it", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  const name = `ci-runner-${unique()}`;
  await page.goto("/settings/tokens");
  await page.getByRole("button", { name: "Create Token" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a Token" });
  await dialog.getByLabel("Name", { exact: true }).fill(name);
  await dialog.getByRole("radio", { name: /^write/ }).check();
  await dialog.getByLabel("Only these Namespaces").fill("ada, ml-team");
  await dialog.getByRole("button", { name: "Create Token" }).click();
  const shown = page.getByRole("dialog", { name: "Copy your new Token" });
  const secret = (await shown.getByTestId("token-secret").textContent()) ?? "";
  // 05-auth.md: hgr_ and 40 base62 characters.
  expect(secret).toMatch(/^hgr_[0-9A-Za-z]{40}$/);
  // The secret is shown once, so neither Escape nor a click outside closes
  // the dialog: only the button that says it was copied.
  await page.keyboard.press("Escape");
  await page.mouse.click(5, 5);
  await expect(shown).toBeVisible();
  await expect(shown.getByRole("button", { name: "Close" })).toHaveCount(0);
  await shown.getByRole("button", { name: "I've copied it" }).click();
  const row = page.getByRole("row").filter({ hasText: name });
  await expect(row).toContainText("write");
  await expect(row).toContainText("ada, ml-team");
  await page.reload();
  await expect(page.getByRole("row").filter({ hasText: name })).toBeVisible();
  expect(await page.content()).not.toContain(secret);
  await page
    .getByRole("row")
    .filter({ hasText: name })
    .getByRole("button", { name: "Revoke" })
    .click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke Token" }).click();
  await expect(page.getByRole("row").filter({ hasText: name })).toHaveCount(0);
  expect(problems).toEqual([]);
});

/**
 * Runs axe on the page as it is, in the light theme and then the dark one,
 * once the colour transitions the switch starts have finished and nothing
 * is hovered.
 */
async function expectAccessibleInBothThemes(page: Page): Promise<void> {
  await page.mouse.move(0, 0);
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    await expectTheme(page, scheme);
    await page.evaluate(() => Promise.all(document.getAnimations().map((a) => a.finished)));
    await expectAccessible(page);
  }
}

test.describe("account", () => {
  test("edits the display name, and a new email waits for its confirmation link", async ({
    page,
  }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto("/settings/profile");
    await expect(page.getByText(/ask an admin for a reset link/i)).toHaveCount(0);
    const save = page.getByRole("button", { name: "Save profile" });
    const email = page.getByRole("textbox", { name: "Email" });
    await expect(save).toBeDisabled();
    await page.getByLabel("Display name").fill("Ada King");
    await email.fill("ada.king@example.com");
    await save.click();
    await expect(
      page.getByRole("status").filter({ hasText: "Your new email waits for confirmation." }),
    ).toBeVisible();
    // The address in use stays until the link is used.
    await expect(email).toHaveValue("ada@example.com");
    const pending = page
      .getByRole("status")
      .filter({ hasText: "waits for confirmation. Ask an admin" });
    await expect(pending).toContainText("ada.king@example.com");
    await page.reload();
    await expect(page.getByLabel("Display name")).toHaveValue("Ada King");
    await expect(pending).toContainText("ada.king@example.com");
    // Another user's email: the admin editor's error.
    await email.fill("grace@example.com");
    await save.click();
    await expect(page.getByRole("alert")).toHaveText(
      "Already exists: the email address is already used.",
    );
    await expectAccessibleInBothThemes(page);

    // An admin creates the confirmation link and sends it to the address.
    await page.emulateMedia({ colorScheme: "light" });
    await page.goto("/admin/users");
    const row = page.getByRole("row").filter({ hasText: "ada.king@example.com" });
    await expect(row).toContainText("Waiting for confirmation");
    await row.getByRole("button", { name: "Email link" }).click();
    const dialog = page.getByRole("dialog", {
      name: "Email confirmation link for ada, to send to ada.king@example.com",
    });
    const url = (await dialog.locator("code").textContent()) ?? "";
    expect(url).toContain("/auth/email/");
    await dialog.getByRole("button", { name: "Done" }).click();

    // Opening the link asks before it confirms; it works once.
    await page.goto(new URL(url).pathname);
    await expect(page.getByRole("heading", { level: 1, name: "Confirm your email" })).toBeVisible();
    await expectAccessibleInBothThemes(page);
    await page.getByRole("button", { name: "Confirm email" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Email confirmed." })).toBeVisible();
    await page.goto("/settings/profile");
    await expect(email).toHaveValue("ada.king@example.com");
    await expect(pending).toHaveCount(0);
    await page.goto(new URL(url).pathname);
    await page.getByRole("button", { name: "Confirm email" }).click();
    await expect(page.getByRole("alert")).toContainText("invalid, used or expired");
    expect(problems).toEqual([]);
  });

  test("changes the password, with errors, and ends other sessions", async ({ page }) => {
    await signIn(page);
    await page.goto("/settings/sessions");
    const signOut = page.getByRole("button", { name: "Sign out", exact: true });
    await expect(signOut).not.toHaveCount(0);
    await page.goto("/settings/profile");
    const current = page.getByLabel("Current password");
    const next = page.getByLabel("New password", { exact: true });
    const confirm = page.getByLabel("Confirm new password");
    const submit = page.getByRole("button", { name: "Change password" });

    await current.fill("wrong-password");
    await next.fill("a fresh passphrase 1");
    await confirm.fill("a fresh passphrase 1");
    await submit.click();
    await expect(current).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByText("The current password is wrong.")).toBeVisible();

    await current.fill("correct-horse-battery");
    await next.fill("short");
    await confirm.fill("short");
    await submit.click();
    await expect(next).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByText("Use at least 10 characters.")).toBeVisible();

    await next.fill("a fresh passphrase 1");
    await confirm.fill("a different passphrase 2");
    await submit.click();
    await expect(page.getByText("The new passwords do not match.")).toBeVisible();
    await expectAccessibleInBothThemes(page);

    await confirm.fill("a fresh passphrase 1");
    await submit.click();
    await expect(page.getByRole("status").filter({ hasText: "Password changed." })).toContainText(
      "Your other sessions are signed out",
    );
    await expect(current).toHaveValue("");
    await expectAccessibleInBothThemes(page);
    await page.goto("/settings/sessions");
    await expect(page.getByText("This browser")).toBeVisible();
    await expect(signOut).toHaveCount(0);
  });

  test("tells users who sign in with a provider that they have no password", async ({ page }) => {
    await page.goto("/auth/login?redirect=%2Fsettings%2Fprofile");
    await page.getByRole("link", { name: "Sign in with UniFi" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Profile" })).toBeVisible();
    await expect(page.getByText("your account has no Hangar password")).toBeVisible();
    await expect(page.getByLabel("Current password")).toHaveCount(0);
    await expect(page.getByLabel("Display name")).toBeEditable();
  });
});

test.describe("admin settings", () => {
  test("changes Instance settings", async ({ page }) => {
    await signIn(page);
    await page.goto("/admin/settings");
    const anon = page.getByRole("switch", { name: "Anonymous read" });
    await expect(anon).not.toBeChecked();
    await anon.click();
    await page.getByLabel("Registration").selectOption("open");
    await page.getByRole("button", { name: "Save settings" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Settings saved." })).toBeVisible();
    await page.reload();
    await expect(page.getByRole("switch", { name: "Anonymous read" })).toBeChecked();
    await expect(page.getByLabel("Registration")).toHaveValue("open");
  });

  test("lists every Organisation and filters them", async ({ page }) => {
    const csrf = await signIn(page);
    // Every Organisation the API lists, rather than a fixed number: other
    // flows can make more.
    const orgs = await callApi(page, csrf, "GET", "/admin/orgs?limit=100");
    expect(orgs.ok(), await orgs.text()).toBe(true);
    const { items } = (await orgs.json()) as { items: unknown[] };
    await page.goto("/admin/organisations");
    const list = page.getByRole("listitem");
    await expect(list.filter({ hasText: "ML team" })).toContainText("2 members");
    await expect(list.filter({ hasText: "TinyLlama" })).toContainText("Created by a Pull");
    await expect(list).toHaveCount(items.length);
    await page.getByLabel("Find an Organisation").fill("tiny");
    await page.getByLabel("Find an Organisation").press("Enter");
    await expect(list).toHaveCount(1);
    await expect(list).toContainText("TinyLlama");
  });

  test("validates Pull windows before saving", async ({ page }) => {
    await signIn(page);
    await page.goto("/admin/pulls");
    await page.getByLabel("Pull windows").fill("1:00-7:00");
    await expect(page.getByText("Not in the form 01:00-07:00: 1:00-7:00")).toBeVisible();
    await expect(page.getByRole("button", { name: "Save Pull settings" })).toBeDisabled();
    await page.getByLabel("Pull windows").fill("01:00-07:00");
    await page.getByLabel("Bandwidth limit (MiB/s)").fill("12.5");
    await page.getByRole("button", { name: "Save Pull settings" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Settings saved." })).toBeVisible();
  });

  test("manages users, Upstreams, Jobs and storage", async ({ page }) => {
    const csrf = await signIn(page);
    // Make a token.created event to find in the audit log.
    const token = await callApi(page, csrf, "POST", "/me/tokens", {
      name: `audit-${unique()}`,
      scope: "read",
    });
    expect(token.status()).toBe(201);
    const { id } = (await token.json()) as { id: string };
    await page.goto("/admin/users");
    await page
      .getByRole("row")
      .filter({ hasText: "grace" })
      .getByRole("button", { name: "Reset link" })
      .click();
    await expect(page.getByRole("dialog", { name: "Password reset link for grace" })).toContainText(
      "/auth/reset/",
    );
    await page.getByRole("button", { name: "Done" }).click();
    await page.goto("/admin/upstreams");
    await page
      .getByRole("listitem")
      .filter({ hasText: "Corporate mirror" })
      .getByRole("button", { name: "Test connection" })
      .click();
    await expect(page.getByRole("status").filter({ hasText: "Failed." })).toContainText(
      "connection refused",
    );
    await page.goto("/admin/jobs?status=failed");
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Job queued again." })).toBeVisible();
    await page.goto("/admin/storage");
    await expect(page.getByRole("row").filter({ hasText: "ml-team" })).toContainText("of 200 GiB");
    // The action filter matches whole actions, as W5's audit query does.
    await page.goto("/admin/audit?action=token.created");
    await expect(page.getByRole("table", { name: "Audit events" })).toContainText("token.created");
    expect((await callApi(page, csrf, "DELETE", `/me/tokens/${id}`)).ok()).toBe(true);
  });
});

test.describe("datasets", () => {
  test("pages, sorts, searches and filters rows in the Viewer", async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto("/datasets/ada/imdb-mini");
    await page
      .getByRole("navigation", { name: "Repo" })
      .getByRole("link", { name: "Viewer" })
      .click();
    await expect(page.getByText("Rows 1 to 100 of 2,000")).toBeVisible();
    await page.getByRole("button", { name: "Next page" }).click();
    await expect(page.getByText("Rows 101 to 200 of 2,000")).toBeVisible();
    await page.getByRole("button", { name: "Sort by rating" }).click();
    await expect(page).toHaveURL(/orderby=/);
    await expect(page.getByRole("columnheader", { name: /rating/ })).toHaveAttribute(
      "aria-sort",
      "ascending",
    );
    await page.getByRole("button", { name: "Filter" }).click();
    const filter = page.getByRole("form", { name: "Filter rows" });
    await filter.getByLabel("Column").selectOption("label");
    await filter.getByLabel("Value").fill("0");
    await expect(filter.getByText('"label" = 0')).toBeVisible();
    await filter.getByRole("button", { name: "Apply filter" }).click();
    await expect(page.getByText(/Rows where/)).toContainText('"label" = 0');
    const firstRow = page.getByRole("table", { name: "Rows" }).getByRole("row").nth(1);
    await expect(firstRow).toContainText("neg (0)");
    await page.getByLabel("Search the rows").fill("triumph");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.getByRole("table", { name: "Rows" }).getByRole("row").nth(1)).toContainText(
      "triumph",
    );
    expect(problems).toEqual([]);
  });

  test("shows the preparing state, then images when the export finishes", async ({ page }) => {
    await signIn(page);
    // A real Parquet export finishes in seconds, so answer the Configs call
    // as Hangar does while the dataset.convert Job runs: every Split pending.
    const isConfigs = (url: URL) =>
      url.pathname === "/api/hangar/v1/repos/datasets/ada/cifar-mini/configs";
    await page.route(isConfigs, async (route) => {
      const response = await route.fetch();
      const body = (await response.json()) as { configs: { splits: { status: string }[] }[] };
      for (const c of body.configs) for (const sp of c.splits) sp.status = "pending";
      await route.fulfill({ response, json: body });
    });
    await page.goto("/datasets/ada/cifar-mini/viewer/default/train");
    await expect(
      page.getByText("Preparing preview. Hangar is converting this dataset to Parquet."),
    ).toBeVisible();
    // The Viewer polls while a Split is pending and shows the rows once the
    // export is ready.
    await page.unroute(isConfigs);
    await expect(page.getByRole("img", { name: "img, row 0" })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText(/^Preparing preview/)).toHaveCount(0);
  });

  test("edits the dataset card with the form", async ({ page }) => {
    await signIn(page);
    await page.goto("/datasets/ada/imdb-mini");
    await page.getByRole("button", { name: "Edit card" }).click();
    await page.getByLabel("Pretty name").fill("IMDb reviews (mini)");
    await page.getByLabel("Languages").fill("en, fr");
    await expect(page.getByRole("region", { name: "Code" })).toContainText(
      "pretty_name: IMDb reviews (mini)",
    );
    await page.getByLabel("Languages").fill("en, French");
    await expect(page.getByRole("list", { name: "Problems to fix" })).toContainText(
      "French is not a language code",
    );
    await page.getByLabel("Languages").fill("en, fr");
    // A Commit runs git on the server, which can take seconds on a loaded
    // host, so wait for the Commit itself rather than a fixed time.
    const committed = commitResponse(page, "datasets/ada/imdb-mini");
    await page.getByRole("button", { name: "Commit changes" }).click();
    expect((await committed).status()).toBe(201);
    await expect(page.getByRole("status").filter({ hasText: "Dataset card saved." })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "About this Repo" })).toContainText(
      "IMDb reviews (mini)",
    );
  });
});

test("searches from anywhere with the keyboard", async ({ page }) => {
  await signIn(page);
  await page.goto("/models");
  await settled(page);
  const dialog = await searchWithKeyboard(page, "imdb");
  await dialog.getByRole("option", { name: /datasets\/ada\/imdb-mini/ }).click();
  await expect(page).toHaveURL(/\/datasets\/ada\/imdb-mini$/);
});

test("the search palette keeps models and datasets in separate groups", async ({ page }) => {
  await signIn(page);
  await page.goto("/models");
  await settled(page);
  // "ada" matches models and datasets in ada's Namespace.
  const dialog = await searchWithKeyboard(page, "ada");
  const models = dialog.getByRole("group", { name: "Models" });
  const datasets = dialog.getByRole("group", { name: "Datasets" });
  await expect(models.getByRole("option", { name: "ada/bert-tiny-finetune" })).toBeVisible();
  await expect(datasets.getByRole("option", { name: "datasets/ada/imdb-mini" })).toBeVisible();
  await expect(models.getByRole("option", { name: /^datasets\// })).toHaveCount(0);
  await expect(datasets.getByRole("option", { name: /^ada\// })).toHaveCount(0);
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.type("datasets");
  await dialog.getByRole("option", { name: "Datasets", exact: true }).click();
  await expect(page).toHaveURL(/\/datasets$/);
  await expect(page.getByRole("heading", { level: 1, name: "Datasets" })).toBeVisible();
});

test("reorders a Collection with the keyboard", async ({ page }) => {
  await signIn(page);
  await page.goto("/collections/ada/small-and-fast");
  await page.getByRole("button", { name: "Edit" }).click();
  const items = page.getByRole("list", { name: "Items" }).getByRole("listitem");
  await expect(items.first()).toContainText("TinyLlama/TinyLlama-1.1B-Chat-GGUF");
  await page
    .getByRole("button", { name: "Move TinyLlama/TinyLlama-1.1B-Chat-GGUF", exact: true })
    .focus();
  // dnd-kit measures between key presses, so give each step a frame or two.
  await page.keyboard.press("Space");
  await page.waitForTimeout(150);
  await page.keyboard.press("ArrowDown");
  await page.waitForTimeout(150);
  await page.keyboard.press("Space");
  await expect(items.first()).toContainText("ada/bert-tiny-finetune");
  await page.reload();
  await expect(
    page.getByRole("list", { name: "Items" }).getByRole("listitem").first(),
  ).toContainText("ada/bert-tiny-finetune");
});

test("creates a Repo and a Collection in an Organisation's Namespace", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto("/new");
  await expect(page.getByLabel("Owner")).toHaveCount(0);
  const id = unique();
  await page.getByLabel("Namespace", { exact: true }).selectOption("ml-team");
  await page.getByLabel("Model name").fill(`laptop-classifier-${id}`);
  await page.getByRole("radio", { name: /^Private/ }).check();
  await expect(page.getByText("Only members of the Namespace and collaborators.")).toBeVisible();
  await page.getByRole("button", { name: "Create model" }).click();
  await expect(page).toHaveURL(new RegExp(`/ml-team/laptop-classifier-${id}$`));

  await page.goto("/collections");
  await page.getByRole("button", { name: "New Collection" }).click();
  const dialog = page.getByRole("dialog", { name: "New Collection" });
  await expect(dialog.getByLabel("Owner")).toHaveCount(0);
  await dialog.getByLabel("Namespace", { exact: true }).selectOption("ml-team");
  await dialog.getByLabel("Title").fill(`Runs on a laptop ${id}`);
  await dialog.getByRole("button", { name: "Create Collection" }).click();
  // The slug is the title in lower case with hyphens (collections.Slugify).
  await expect(page).toHaveURL(new RegExp(`/collections/ml-team/runs-on-a-laptop-${id}$`));
  await expect(
    page.getByRole("heading", { level: 1, name: `Runs on a laptop ${id}` }),
  ).toBeVisible();
  await expect(page.getByText("Choose Edit to add Repos.")).toBeVisible();
  expect(problems).toEqual([]);
});

test("shows the empty Models and Datasets lists and virtualises long lists", async ({ page }) => {
  // The mock makes the 260 Repos for this session, which takes a few seconds.
  test.setTimeout(60_000);
  const csrf = await signIn(page);
  // An Instance with Repos in it cannot have an empty arsenal, so answer the
  // lists' calls with an empty page.
  const isList = (url: URL) => url.pathname === "/api/hangar/v1/repos";
  await page.route(isList, (route) =>
    route.request().method() === "GET"
      ? route.fulfill({ json: { items: [], facets: {} } })
      : route.fallback(),
  );
  await page.goto("/models");
  await expect(page.getByText("Your arsenal has no models yet.")).toBeVisible();
  await expect(page.getByRole("link", { name: "Discover a model" })).toHaveAttribute(
    "href",
    "/discover",
  );
  await page.goto("/datasets");
  await expect(page.getByText("Your arsenal has no datasets yet.")).toBeVisible();
  await expect(page.getByRole("link", { name: "Discover a dataset" })).toHaveAttribute(
    "href",
    "/discover?type=datasets",
  );
  await expect(page.getByRole("link", { name: "Upload your own" })).toHaveAttribute(
    "href",
    "/new-dataset",
  );
  await page.unroute(isList);

  await seedBulkRepos(page, csrf);
  await page.goto("/models?ns=bulk&view=list&sort=name");
  const list = page.getByRole("list", { name: "Models" });
  await expect(list.getByRole("link", { name: "bulk/bulk-model-000" })).toBeVisible();
  // Load every page: by button, or by scrolling to the end of the list.
  const last = list.getByRole("link", { name: "bulk/bulk-model-259" });
  const more = page.getByRole("button", { name: "Load more" });
  await expect(async () => {
    // Scrolling to the end can load the last page between isVisible and
    // click, and then the button unmounts for good. A click without a
    // timeout would wait for it until toPass runs out, so give the click a
    // short timeout and let the next iteration check the list instead.
    if (await more.isVisible()) await more.click({ timeout: 1_000 }).catch(() => {});
    await page.keyboard.press("End");
    await expect(last).toBeVisible({ timeout: 500 });
  }).toPass({ timeout: 45_000 });
  // The last page is in, so there is nothing more to load.
  await expect(more).toHaveCount(0);
  // 260 Repos are loaded, but only the rows near the screen are in the page.
  const rendered = await list.locator("article").count();
  expect(rendered).toBeGreaterThan(0);
  expect(rendered).toBeLessThan(200);
});

/** The text of the label for a checkbox. */
async function labelOf(page: Page, checkbox: ReturnType<Page["getByRole"]>): Promise<string> {
  const id = await checkbox.getAttribute("id");
  expect(id).toBeTruthy();
  return page.locator(`label[for="${id}"]`).innerText();
}
