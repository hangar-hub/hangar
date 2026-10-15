import { expect, type Route, test } from "@playwright/test";
import { expectAccessible, settled, signIn, useScenario, watchConsole } from "./helpers";

// The flows 15-build-plan.md asks W8 to cover: sign-in, Discover and Pull,
// upload, browsing files and the GGUF viewer, Token creation and admin
// settings; plus the dataset Viewer, card editor, search and Collections.

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
    await expect(page.getByRole("heading", { level: 1, name: "Arsenal" })).toBeVisible();
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

  test("explains OIDC errors", async ({ page }) => {
    await page.goto("/auth/login?error=oidc_link_required");
    await expect(page.getByRole("alert")).toContainText("link the provider from Settings");
  });

  test("accepts an invite", async ({ page }) => {
    await page.goto("/auth/invite/abc123");
    await page.getByLabel("Username").fill("grace2");
    await page.getByLabel("Password", { exact: true }).fill("short");
    await page.getByLabel("Confirm password").fill("short");
    await page.getByRole("button", { name: "Create account" }).click();
    await expect(page.getByText("Use at least 10 characters.")).toBeVisible();
    await page.getByLabel("Password", { exact: true }).fill("a-long-enough-password");
    await page.getByLabel("Confirm password").fill("a-long-enough-password");
    await page.getByRole("button", { name: "Create account" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Arsenal" })).toBeVisible();
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
  await panel.getByRole("checkbox", { name: /Q8_0/ }).check();
  await expect(panel.getByText(/To download:/)).toContainText("1.1 GiB");
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
  await panel.getByRole("checkbox", { name: "train" }).check();
  await panel.getByRole("checkbox", { name: "Parquet export only" }).check();
  await expect(panel.getByText(/To download:/)).toContainText("21.0 MiB");
  await panel.getByRole("tab", { name: "Preview" }).click();
  await expect(panel.getByRole("table", { name: "First rows" })).toContainText(
    "A wonderful little production.",
  );
  await panel.getByRole("button", { name: "Pull into arsenal" }).click();
  await expect(panel.getByRole("button", { name: "Pull started" })).toBeDisabled();
});

test.describe("upload", () => {
  test("uploads small and large files in the browser and commits them", async ({ page }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    await page.getByRole("button", { name: "Upload files" }).click();
    await page.getByLabel("Choose files").setInputFiles([
      { name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("hello from the browser") },
      {
        name: "weights.safetensors",
        mimeType: "application/octet-stream",
        buffer: Buffer.alloc(6 * 1024 * 1024, 7),
      },
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
    await expect(files.getByRole("link", { name: "weights.safetensors" })).toBeVisible();
    await expect(files.getByRole("row").filter({ hasText: "weights.safetensors" })).toContainText(
      "LFS",
    );
    await files.getByRole("link", { name: "notes.txt" }).click();
    await expect(page.getByRole("region", { name: "Code" })).toContainText(
      "hello from the browser",
    );
    expect(problems).toEqual([]);
  });

  test("resumes an upload after a page reload, sending only the missing parts", async ({
    page,
  }) => {
    const problems = watchConsole(page);
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    const weights = {
      name: "resume.safetensors",
      mimeType: "application/octet-stream",
      buffer: Buffer.alloc(6 * 1024 * 1024, 9),
    };
    // Hold part 2 so the page is reloaded with part 1 stored and part 2 not.
    let held: Route | undefined;
    await page.route("**/uploads/*/parts/2", (route) => {
      held = route;
    });
    const part1 = page.waitForResponse(
      (r) => r.request().method() === "PUT" && /\/uploads\/[^/]+\/parts\/1$/.test(r.url()),
    );
    await page.getByRole("button", { name: "Upload files" }).click();
    await page.getByLabel("Choose files").setInputFiles(weights);
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

    // The plan starts a new upload, as W2's does; the browser resumes the first.
    const puts: string[] = [];
    page.on("request", (r) => {
      if (r.method() === "PUT") puts.push(new URL(r.url()).pathname);
    });
    await page.getByRole("button", { name: "Upload files" }).click();
    await page.getByLabel("Choose files").setInputFiles(weights);
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

  test("opens a pull request from an upload", async ({ page }) => {
    await signIn(page);
    await page.goto("/ada/bert-tiny-finetune/tree/main");
    await page.getByRole("button", { name: "Upload files" }).click();
    await page.getByLabel("Choose files").setInputFiles({
      name: "extra.md",
      mimeType: "text/markdown",
      buffer: Buffer.from("# Extra\n"),
    });
    await page.getByLabel("Open as pull request").check();
    await page.getByRole("button", { name: "Upload and open pull request" }).click();
    await expect(page).toHaveURL(/\/ada\/bert-tiny-finetune\/discussions\/3$/);
    await expect(page.getByRole("heading", { level: 2 })).toContainText("Upload extra.md");
    await page.getByRole("tab", { name: /Files changed/ }).click();
    await expect(page.getByRole("list", { name: "Changed files" })).toContainText("extra.md");
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
    await page.getByRole("button", { name: "Commit to main" }).click();
    await expect(
      page.getByRole("status").filter({ hasText: "Saved tokenizer_config.json." }),
    ).toBeVisible();
    await page.getByRole("tab", { name: "Source" }).click();
    await expect(page.getByRole("region", { name: "Code" })).toContainText(
      '"do_lower_case": false',
    );
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
  await page.goto("/settings/tokens");
  await page.getByRole("button", { name: "Create Token" }).click();
  const dialog = page.getByRole("dialog", { name: "Create a Token" });
  await dialog.getByLabel("Name", { exact: true }).fill("ci-runner");
  await dialog.getByRole("radio", { name: /^write/ }).check();
  await dialog.getByLabel("Only these Namespaces").fill("ada, ml-team");
  await dialog.getByRole("button", { name: "Create Token" }).click();
  const shown = page.getByRole("dialog", { name: "Copy your new Token" });
  const secret = (await shown.getByTestId("token-secret").textContent()) ?? "";
  expect(secret).toMatch(/^hgr_[0-9a-f]{40}$/);
  await shown.getByRole("button", { name: "I've copied it" }).click();
  const row = page.getByRole("row").filter({ hasText: "ci-runner" });
  await expect(row).toContainText("write");
  await expect(row).toContainText("ada, ml-team");
  await page.reload();
  await expect(page.getByRole("row").filter({ hasText: "ci-runner" })).toBeVisible();
  expect(await page.content()).not.toContain(secret);
  await page
    .getByRole("row")
    .filter({ hasText: "ci-runner" })
    .getByRole("button", { name: "Revoke" })
    .click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke Token" }).click();
  await expect(page.getByRole("row").filter({ hasText: "ci-runner" })).toHaveCount(0);
  expect(problems).toEqual([]);
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
    await signIn(page);
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
    await page.goto("/admin/audit?action=token");
    await expect(page.getByRole("table", { name: "Audit events" })).toContainText("token.created");
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

  test("shows images and the preparing state", async ({ page }) => {
    await signIn(page);
    await page.goto("/datasets/ada/cifar-mini/viewer/default/train");
    await expect(page.getByRole("img", { name: "img, row 0" })).toBeVisible();
    await page.goto("/datasets/ml-team/speech-commands-mini/viewer");
    await expect(
      page.getByText("Preparing preview. Hangar is converting this dataset to Parquet."),
    ).toBeVisible();
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
    await page.getByRole("button", { name: "Commit changes" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Dataset card saved." })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "About this Repo" })).toContainText(
      "IMDb reviews (mini)",
    );
  });
});

test("searches from anywhere with the keyboard", async ({ page }) => {
  await signIn(page);
  await page.goto("/arsenal");
  await settled(page);
  const dialog = page.getByRole("dialog", { name: "Search" });
  await expect(async () => {
    await page.keyboard.press("/");
    await expect(dialog).toBeVisible({ timeout: 1000 });
  }).toPass();
  await dialog.getByRole("combobox", { name: "Search Hangar" }).fill("imdb");
  await dialog.getByRole("option", { name: /datasets\/ada\/imdb-mini/ }).click();
  await expect(page).toHaveURL(/\/datasets\/ada\/imdb-mini$/);
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
  await page.getByLabel("Namespace", { exact: true }).selectOption("ml-team");
  await page.getByLabel("Model name").fill("laptop-classifier");
  await page.getByRole("radio", { name: /^Private/ }).check();
  await expect(page.getByText("Only members of the Namespace and collaborators.")).toBeVisible();
  await page.getByRole("button", { name: "Create model" }).click();
  await expect(page).toHaveURL(/\/ml-team\/laptop-classifier$/);

  await page.goto("/collections");
  await page.getByRole("button", { name: "New Collection" }).click();
  const dialog = page.getByRole("dialog", { name: "New Collection" });
  await expect(dialog.getByLabel("Owner")).toHaveCount(0);
  await dialog.getByLabel("Namespace", { exact: true }).selectOption("ml-team");
  await dialog.getByLabel("Title").fill("Runs on a laptop");
  await dialog.getByRole("button", { name: "Create Collection" }).click();
  await expect(page).toHaveURL(/\/collections\/ml-team\/runs-on-a-laptop-[0-9a-f]{6}$/);
  await expect(page.getByRole("heading", { level: 1, name: "Runs on a laptop" })).toBeVisible();
  await expect(page.getByText("Choose Edit to add Repos.")).toBeVisible();
  expect(problems).toEqual([]);
});

test("shows the empty arsenal and virtualises long lists", async ({ page, baseURL }) => {
  await useScenario(page, "empty", baseURL ?? "");
  await signIn(page);
  await page.goto("/arsenal");
  await expect(page.getByText("Your arsenal is empty.")).toBeVisible();
  await page.context().clearCookies();
  await useScenario(page, "bulk", baseURL ?? "");
  await signIn(page);
  await page.goto("/arsenal?ns=bulk&view=list");
  const list = page.getByRole("list", { name: "Repos" });
  await expect(list.getByRole("link", { name: "bulk/bulk-model-000" })).toBeVisible();
  // Load every page: by button, or by scrolling to the end of the list.
  const last = list.getByRole("link", { name: "bulk/bulk-model-259" });
  await expect(async () => {
    const more = page.getByRole("button", { name: "Load more" });
    if (await more.isVisible()) await more.click();
    await page.keyboard.press("End");
    await expect(last).toBeVisible({ timeout: 500 });
  }).toPass({ timeout: 45_000 });
  // 260 Repos are loaded, but only the rows near the screen are in the page.
  const rendered = await list.locator("article").count();
  expect(rendered).toBeGreaterThan(0);
  expect(rendered).toBeLessThan(200);
});
