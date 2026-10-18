import { expect, test } from "@playwright/test";
import {
  expectAccessible,
  expectGlossaryTerms,
  expectNoHorizontalOverflow,
  expectTheme,
  settled,
  watchConsole,
  watchFailedLoads,
} from "../e2e/helpers";
import { env, type Session, session, signIn, upstreamName, user } from "./helpers";
import { collection, datasetRepo, modelRepo } from "./seed";

// Every page renders against a real Instance with one h1, fits the width
// without scrolling sideways, passes every axe rule, avoids the glossary's
// "Avoid" terms and logs no CSP violation, at the phone, tablet and desktop
// widths, as e2e/pages.spec.ts checks against the mock. The pages show the
// Repos, Collection and discussion that seed.ts makes.

const adminPassword = () => env("HANGAR_E2E_ADMIN_PASSWORD");

/** The newest Commit of a Repo's main branch. */
async function head(s: Session, repo: string): Promise<string> {
  const res = await s.call("GET", `/repos/${repo}/commits?limit=1`);
  expect(res.ok(), await res.text()).toBe(true);
  const sha = (await res.json()).items[0]?.sha as string | undefined;
  if (!sha) throw new Error(`${repo} has no Commits`);
  return sha;
}

/** The number of a Repo's first discussion. */
async function firstDiscussion(s: Session, repo: string): Promise<number> {
  const res = await s.call("GET", `/repos/${repo}/discussions`);
  expect(res.ok(), await res.text()).toBe(true);
  const items = (await res.json()).items as { num: number }[];
  const num = Math.min(...items.map((d) => d.num));
  if (!Number.isFinite(num)) throw new Error(`${repo} has no discussions`);
  return num;
}

type Route = {
  /** The path, or how to work it out from the Instance's data. */
  path: string | ((s: Session) => Promise<string>);
  title?: string;
  as?: "admin";
};

const models = `models/${modelRepo}`;
const datasets = `datasets/${datasetRepo}`;

const signedIn: Route[] = [
  { path: "/" },
  { path: "/models" },
  { path: "/models?view=list" },
  { path: "/datasets" },
  { path: "/datasets?view=list" },
  { path: `/discover?upstream=${upstreamName}` },
  { path: `/discover?type=datasets&upstream=${upstreamName}` },
  { path: `/${modelRepo}` },
  { path: `/${modelRepo}/tree/main` },
  { path: `/${modelRepo}/blob/main/README.md` },
  { path: `/${modelRepo}/blob/main/config.json` },
  { path: `/${modelRepo}/blob/main/model.safetensors` },
  { path: `/${modelRepo}/blob/main/evil.pkl` },
  { path: `/${modelRepo}/commits/main` },
  {
    title: `/${modelRepo}/commit/<head of main>`,
    path: async (s) => `/${modelRepo}/commit/${await head(s, models)}`,
  },
  { path: `/${modelRepo}/discussions` },
  {
    title: `/${modelRepo}/discussions/<first>`,
    path: async (s) => `/${modelRepo}/discussions/${await firstDiscussion(s, models)}`,
  },
  { path: `/${modelRepo}/settings` },
  { path: `/datasets/${datasetRepo}` },
  { path: `/datasets/${datasetRepo}/viewer` },
  { path: `/datasets/${datasetRepo}/tree/main` },
  { path: `/datasets/${datasetRepo}/blob/main/data/train.csv` },
  { path: `/datasets/${datasetRepo}/commits/main` },
  {
    title: `/datasets/${datasetRepo}/commit/<head of main>`,
    path: async (s) => `/datasets/${datasetRepo}/commit/${await head(s, datasets)}`,
  },
  { path: `/datasets/${datasetRepo}/discussions` },
  { path: `/datasets/${datasetRepo}/settings` },
  { path: "/new" },
  { path: "/new-dataset" },
  { path: `/${user.username}` },
  { path: "/collections" },
  { path: `/collections/${user.username}/${collection.slug}` },
  { path: "/settings" },
  { path: "/settings/profile" },
  { path: "/settings/tokens" },
  { path: "/settings/ollama-keys" },
  { path: "/settings/upstream-tokens" },
  { path: "/settings/sessions" },
  { path: "/settings/organisations" },
  { path: "/no-such-namespace" },
  { path: "/admin", as: "admin" },
  { path: "/admin/system", as: "admin" },
  { path: "/admin/users", as: "admin" },
  { path: "/admin/organisations", as: "admin" },
  { path: "/admin/upstreams", as: "admin" },
  { path: "/admin/pulls", as: "admin" },
  { path: "/admin/storage", as: "admin" },
  { path: "/admin/jobs", as: "admin" },
  { path: "/admin/webhooks", as: "admin" },
  { path: "/admin/audit", as: "admin" },
  { path: "/admin/settings", as: "admin" },
];

const signedOut = ["/", "/auth/login", "/auth/login?error=oidc_denied"];

// Spec 09 asks for axe in both themes on every page: the light theme runs
// every check, and the dark theme runs axe on the same pages.
const schemes = ["light", "dark"] as const;

for (const route of signedIn) {
  const title = route.title ?? (route.path as string);
  for (const scheme of schemes) {
    const name =
      scheme === "light"
        ? `${title} renders, fits the width and is accessible${route.as ? " for admins" : ""}`
        : `${title} passes axe in the dark theme${route.as ? " for admins" : ""}`;
    test(name, async ({ page }) => {
      const problems = watchConsole(page);
      await page.emulateMedia({ colorScheme: scheme });
      const [username, password] =
        route.as === "admin" ? ["hangar-admin", adminPassword()] : [user.username, user.password];
      let path = route.path;
      if (typeof path !== "string") {
        const s = await session(env("HANGAR_URL"), username, password);
        try {
          path = await path(s);
        } finally {
          await s.dispose();
        }
      }
      await signIn(page, username, password);
      await page.goto(path);
      await expect(page.locator("h1")).toHaveCount(1);
      await settled(page);
      // Again once lazy content such as a Markdown card has loaded.
      await expect(page.locator("h1")).toHaveCount(1);
      await expectTheme(page, scheme);
      await expectNoHorizontalOverflow(page);
      await expectAccessible(page);
      await expectGlossaryTerms(page);
      expect(problems).toEqual([]);
    });
  }
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
