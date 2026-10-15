import { defineConfig, devices } from "@playwright/test";

// End-to-end flows against a real, running Hangar (e2e-real/), seeded
// through the API by e2e-real/seed.ts. Set HANGAR_URL to the Instance and
// HANGAR_E2E_ADMIN_PASSWORD to its bootstrap admin password, for example:
//
//   HANGAR_URL=http://localhost:8080 HANGAR_E2E_ADMIN_PASSWORD=... \
//     corepack pnpm exec playwright test -c playwright.real.config.ts
const baseURL = process.env.HANGAR_URL;
if (!baseURL) throw new Error("Set HANGAR_URL to a running Hangar for the e2e-real suite.");

export default defineConfig({
  testDir: "./e2e-real",
  globalSetup: "./e2e-real/seed.ts",
  fullyParallel: false,
  workers: 1,
  forbidOnly: Boolean(process.env.CI),
  retries: 0,
  reporter: process.env.CI
    ? [["github"], ["html", { open: "never", outputFolder: "playwright-report-real" }]]
    : "list",
  use: { baseURL, trace: "retain-on-failure" },
  projects: [
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
    },
    { name: "phone", use: { ...devices["Pixel 7"], viewport: { width: 390, height: 844 } } },
  ],
});
