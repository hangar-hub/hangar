import { defineConfig, devices } from "@playwright/test";

// End-to-end flows and page checks against a real, running Hangar
// (e2e-real/), seeded through the API by e2e-real/seed.ts. They need two
// Instances: the one under test, and a second one it adds as its Upstream
// for the Discover and Pull flows. Set HANGAR_URL and
// HANGAR_E2E_ADMIN_PASSWORD (its bootstrap admin password), and
// HANGAR_E2E_UPSTREAM_URL and HANGAR_E2E_UPSTREAM_ADMIN_PASSWORD for the
// second, for example:
//
//   HANGAR_URL=http://localhost:8080 HANGAR_E2E_ADMIN_PASSWORD=... \
//   HANGAR_E2E_UPSTREAM_URL=http://localhost:8081 HANGAR_E2E_UPSTREAM_ADMIN_PASSWORD=... \
//     corepack pnpm exec playwright test -c playwright.real.config.ts
//
// The flows change the Instance, so everything runs one test at a time, one
// width after another.
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
    {
      name: "tablet",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1024, height: 768 } },
    },
    { name: "phone", use: { ...devices["Pixel 7"], viewport: { width: 390, height: 844 } } },
  ],
});
