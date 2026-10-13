import { defineConfig, devices } from "@playwright/test";

// End-to-end tests run against the mock server in mock/ (generated from
// api/openapi.yaml), serving the production build under the real Content
// Security Policy. Set HANGAR_URL to run them against a running Hangar
// instead, such as the Compose quickstart; the flows expect the mock's
// fixture data, so seed an Instance with it first (docs/build/handoffs/w8.md).
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
