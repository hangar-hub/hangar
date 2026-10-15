import { defineConfig, devices } from "@playwright/test";

// End-to-end tests run against the mock server in mock/ (generated from
// api/openapi.yaml), serving the production build under the real Content
// Security Policy. Each browser session gets its own copy of the mock's
// fixture data (docs/build/handoffs/w8.md), so the tests are independent of
// each other and run at the three widths at once. The flows against a real
// Hangar are in e2e-real/, which seeds its own data: run them with
// playwright.real.config.ts (make e2e-real).
if (process.env.HANGAR_URL) {
  throw new Error(
    "e2e/ runs against the mock server, whose fixture data a real Instance does not have. Unset HANGAR_URL, or run the e2e-real suite against it: make e2e-real (playwright test -c playwright.real.config.ts).",
  );
}
const baseURL = "http://127.0.0.1:4174";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : 4,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : "list",
  use: { baseURL, trace: "retain-on-failure" },
  webServer: {
    command: "vite build && vite preview --host 127.0.0.1 --port 4174 --strictPort",
    env: { HANGAR_MOCK: "1" },
    url: `${baseURL}/`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
  projects: [
    { name: "phone", use: { ...devices["Pixel 7"], viewport: { width: 390, height: 844 } } },
    {
      name: "tablet",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1024, height: 768 } },
    },
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
    },
  ],
});
