import { defineConfig, devices } from "@playwright/test";

// End-to-end tests run against the mock server in mock/ (generated from
// api/openapi.yaml), serving the production build under the real Content
// Security Policy. Set HANGAR_URL to run them against a running Hangar
// instead, such as the Compose quickstart. The flows expect the Repos and
// users in the mock's fixture data (docs/build/handoffs/w8.md); they make
// everything else they need through the API, and the `seed` project makes
// the Repos they share once, before the three widths run.
const external = process.env.HANGAR_URL;
const baseURL = external ?? "http://127.0.0.1:4174";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : 4,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : "list",
  use: { baseURL, trace: "retain-on-failure" },
  webServer: external
    ? undefined
    : {
        command: "vite build && vite preview --host 127.0.0.1 --port 4174 --strictPort",
        env: { HANGAR_MOCK: "1" },
        url: "http://127.0.0.1:4174/",
        reuseExistingServer: !process.env.CI,
        timeout: 120_000,
      },
  projects: [
    { name: "seed", testMatch: /seed\.setup\.ts/, use: { ...devices["Desktop Chrome"] } },
    {
      name: "phone",
      use: { ...devices["Pixel 7"], viewport: { width: 390, height: 844 } },
      dependencies: ["seed"],
    },
    {
      name: "tablet",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1024, height: 768 } },
      dependencies: ["seed"],
    },
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
      dependencies: ["seed"],
    },
  ],
});
