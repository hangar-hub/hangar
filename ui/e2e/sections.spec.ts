import { expect, test } from "@playwright/test";
import {
  commitResponse,
  expectAccessible,
  expectNoHorizontalOverflow,
  settled,
  signIn,
  watchConsole,
} from "./helpers";

// Models and Datasets are separate top-level sections with optional links
// between them, decided by James on 2026-10-16 (docs/build/decisions.md).

test("the Models list shows only models and the Datasets list only datasets", async ({ page }) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto("/models");
  await settled(page);
  const models = page.getByRole("list", { name: "Models" });
  await expect(models.getByRole("link", { name: "ada/bert-tiny-finetune" })).toBeVisible();
  await expect(models.getByRole("link", { name: /^datasets\// })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "New model" })).toHaveAttribute("href", "/new");
  await expect(page.getByRole("link", { name: "New dataset" })).toHaveCount(0);

  await page.goto("/datasets");
  await settled(page);
  await expect(page.getByRole("heading", { level: 1, name: "Datasets" })).toBeVisible();
  const datasets = page.getByRole("list", { name: "Datasets" });
  await expect(datasets.getByRole("link", { name: "datasets/ada/imdb-mini" })).toBeVisible();
  await expect(datasets.getByRole("link", { name: "datasets/ada/ingest-samples" })).toBeVisible();
  await expect(datasets.getByRole("link", { name: /^(?!datasets\/)/ })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "New dataset" })).toHaveAttribute(
    "href",
    "/new-dataset",
  );
  await expect(page.getByRole("link", { name: "New model" })).toHaveCount(0);
  // A long ID such as datasets/ml-team/speech-commands-mini is cut short,
  // never widening its card past its cell.
  const overflow = await datasets
    .locator("article")
    .evaluateAll((els) =>
      els
        .filter(
          (el) =>
            el.getBoundingClientRect().right >
            (el.parentElement?.getBoundingClientRect().right ?? 0) + 0.5,
        )
        .map((el) => el.querySelector("h3")?.textContent),
    );
  expect(overflow).toEqual([]);
  // Each list has the filters for its kind only.
  const filters = page.getByRole("complementary", { name: "Filters" });
  if (await filters.isVisible()) {
    await expect(filters.getByText("Modality")).toBeVisible();
    await expect(filters.getByText("Quantisation")).toHaveCount(0);
    await expect(filters.getByText("Parameters")).toHaveCount(0);
    await expect(filters.getByRole("group", { name: "Type" })).toHaveCount(0);
  }
  expect(problems).toEqual([]);
});

test("old Arsenal links go to the list for their type and keep their filters", async ({ page }) => {
  await signIn(page);
  await page.goto("/arsenal?ns=ada&view=list&sort=name");
  await expect(page).toHaveURL(/\/models\?ns=ada&view=list&sort=name$/);
  await expect(page.getByRole("heading", { level: 1, name: "Models" })).toBeVisible();
  await expect(page.getByRole("button", { name: "List", pressed: true })).toBeVisible();

  await page.goto("/arsenal?type=datasets&sort=name");
  await expect(page).toHaveURL(/\/datasets\?sort=name$/);
  await expect(page.getByRole("heading", { level: 1, name: "Datasets" })).toBeVisible();
});

test("a model lists the datasets it uses, and each dataset lists it under Used by", async ({
  page,
}) => {
  const problems = watchConsole(page);
  await signIn(page);
  // ml-team/phi-mini-GGUF uses no dataset yet, and its card has no metadata.
  await page.goto("/ml-team/phi-mini-GGUF");
  await settled(page);
  const about = page.getByRole("complementary", { name: "About this Repo" });
  await expect(about.getByText("No datasets listed.")).toBeVisible();
  await about.getByRole("link", { name: "Choose datasets" }).click();
  await expect(page).toHaveURL(/\/ml-team\/phi-mini-GGUF\/settings$/);
  const section = page.getByRole("region", { name: "Datasets used" });
  await expect(section.getByText("No datasets listed.")).toBeVisible();

  // Pick one from the arsenal, and name one on an Upstream by its ID.
  await section.getByLabel("Find a dataset").fill("imdb");
  await section.getByRole("button", { name: "Add datasets/ada/imdb-mini" }).click();
  await section.getByLabel("Find a dataset").fill("squad");
  await section.getByRole("button", { name: "Add squad by ID" }).click();
  const chosen = section.getByRole("list", { name: "Datasets used" });
  await expect(chosen.getByRole("listitem")).toHaveText(["ada/imdb-mini", "squad"]);
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);

  const committed = commitResponse(page, "models/ml-team/phi-mini-GGUF");
  await section.getByRole("button", { name: "Commit to the Model card" }).click();
  expect((await committed).status()).toBe(201);
  await expect(page.getByRole("status").filter({ hasText: "Datasets used saved." })).toBeVisible();

  // The card's metadata is the Hub's: a datasets list, the body kept.
  const card = await (
    await page.request.get("/ml-team/phi-mini-GGUF/resolve/main/README.md")
  ).text();
  expect(card).toBe(
    "---\ndatasets:\n  - ada/imdb-mini\n  - squad\n---\n\n# phi-mini GGUF\n\nQuantised builds for laptops.\n",
  );

  await page.goto("/ml-team/phi-mini-GGUF");
  await expect(
    about
      .getByRole("region", { name: "Datasets used" })
      .getByRole("link", { name: "ada/imdb-mini" }),
  ).toBeVisible();
  await expect(about.getByRole("region", { name: "Datasets used" })).toContainText("squad");

  await page.goto("/datasets/ada/imdb-mini");
  const usedBy = page
    .getByRole("complementary", { name: "About this Repo" })
    .getByRole("region", { name: "Used by" });
  await expect(usedBy.getByRole("link", { name: "ada/bert-tiny-finetune" })).toBeVisible();
  await expect(usedBy.getByRole("link", { name: "ml-team/phi-mini-GGUF" })).toBeVisible();

  // Taking the link away again removes the model from Used by.
  await page.goto("/ml-team/phi-mini-GGUF/settings");
  await section.getByRole("button", { name: "Remove ada/imdb-mini" }).click();
  const again = commitResponse(page, "models/ml-team/phi-mini-GGUF");
  await section.getByRole("button", { name: "Commit to the Model card" }).click();
  expect((await again).status()).toBe(201);
  await page.goto("/datasets/ada/imdb-mini");
  await expect(usedBy.getByRole("link", { name: "ada/bert-tiny-finetune" })).toBeVisible();
  await expect(usedBy.getByRole("link", { name: "ml-team/phi-mini-GGUF" })).toHaveCount(0);
  expect(problems).toEqual([]);
});

test("a dataset of other files reads well and the Viewer says why it shows nothing", async ({
  page,
}) => {
  const problems = watchConsole(page);
  await signIn(page);
  await page.goto("/datasets/ada/ingest-samples");
  await settled(page);
  // No links and no Configs is normal: no error, no Used by.
  const about = page.getByRole("complementary", { name: "About this Repo" });
  await expect(about.getByRole("region", { name: "Configs and Splits" })).toContainText(
    "The Viewer has no data files it can read here",
  );
  await expect(about.getByRole("region", { name: "Used by" })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);

  await page
    .getByRole("navigation", { name: "Repo" })
    .getByRole("link", { name: "Viewer" })
    .click();
  await expect(page.getByText("Nothing to preview")).toBeVisible();
  await expect(page.getByText("This dataset has none it can read")).toBeVisible();
  await expect(page.getByRole("button", { name: "Try again" })).toHaveCount(0);
  await page.getByRole("link", { name: "Browse the files" }).click();
  await expect(page).toHaveURL(/\/datasets\/ada\/ingest-samples\/tree\/main$/);
  await expect(page.getByRole("link", { name: "fixtures" })).toBeVisible();

  // Use this dataset offers the download snippets, not load_dataset.
  await page.getByRole("button", { name: "Use this dataset" }).click();
  const dialog = page.getByRole("dialog", { name: "Use this dataset" });
  await expect(dialog.getByRole("tab")).toHaveText(["hf CLI", "huggingface_hub", "git"]);
  await expect(dialog.getByRole("tabpanel")).toContainText(
    "hf download ada/ingest-samples --repo-type dataset",
  );
  await expectNoHorizontalOverflow(page);
  await expectAccessible(page);
  expect(problems).toEqual([]);
});
