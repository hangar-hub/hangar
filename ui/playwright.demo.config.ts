import { defineConfig, devices } from "@playwright/test";

// Runs the mock suite in e2e/ against the demo Worker under wrangler dev, to
// show the Worker serves what `vite preview` serves, and the demo's own
// checks in e2e-demo/. Build the demo first: make test-demo does both.
// The suite's Worker starts visitors signed out, as `vite preview` does, and
// takes request bodies up to 64 MiB, because the upload flows send a 6 MiB
// file in one part. The demo's own checks use a second Worker with the settings
// demo.hangar.page has: visitors start signed in, and bodies over 1 MiB get 413.
const suitePort = Number(process.env.DEMO_SUITE_PORT ?? 8791);
const demoPort = Number(process.env.DEMO_PORT ?? 8792);
const suiteURL = `http://127.0.0.1:${suitePort}`;
const demoURL = `http://127.0.0.1:${demoPort}`;

const wrangler = (port: number, inspector: number, signedIn: 0 | 1, flags = "") => ({
  command: `make -C .. --no-print-directory demo-run DEMO_PORT=${port} DEMO_INSPECTOR_PORT=${inspector} DEMO_SIGNED_IN=${signedIn} DEMO_DEV_FLAGS="${flags}"`,
  url: `http://127.0.0.1:${port}/assets/hangar.svg`,
  reuseExistingServer: !process.env.CI,
  timeout: 180_000,
});

export default defineConfig({
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : 4,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : "list",
  use: { trace: "retain-on-failure" },
  webServer: [
    wrangler(
      suitePort,
      Number(process.env.DEMO_SUITE_INSPECTOR_PORT ?? 9331),
      0,
      "--var DEMO_TEST_LIMIT_MIB:64",
    ),
    wrangler(demoPort, Number(process.env.DEMO_INSPECTOR_PORT ?? 9332), 1),
  ],
  projects: [
    {
      name: "phone",
      testDir: "./e2e",
      use: { ...devices["Pixel 7"], viewport: { width: 390, height: 844 }, baseURL: suiteURL },
    },
    {
      name: "tablet",
      testDir: "./e2e",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1024, height: 768 },
        baseURL: suiteURL,
      },
    },
    {
      name: "desktop",
      testDir: "./e2e",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1440, height: 900 },
        baseURL: suiteURL,
      },
    },
    {
      name: "demo",
      testDir: "./e2e-demo",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1440, height: 900 },
        baseURL: demoURL,
      },
    },
  ],
});
