/// <reference types="vitest/config" />

import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

/**
 * With HANGAR_MOCK=1, `vite` and `vite preview` answer the API from the
 * mock server in mock/, generated from api/openapi.yaml, instead of a
 * running Hangar. Preview also sends internal/web's Content Security
 * Policy, so Playwright checks the build under the real policy.
 */
function mockApi(): Plugin {
  const load = async () => {
    const { createMock } = await import("./mock/server");
    return createMock({
      signedIn: process.env.HANGAR_MOCK_SIGNED_IN === "1",
      delay: Number(process.env.HANGAR_MOCK_DELAY ?? 0),
    });
  };
  return {
    name: "hangar-mock-api",
    async configureServer(server) {
      server.middlewares.use(await load());
    },
    async configurePreviewServer(server) {
      const { csp } = await import("./mock/server");
      server.middlewares.use((_req, res, next) => {
        res.setHeader("Content-Security-Policy", csp);
        next();
      });
      server.middlewares.use(await load());
    },
  };
}

const mock = process.env.HANGAR_MOCK === "1";

// The UI builds into internal/web/dist, which the Go binary embeds.
export default defineConfig({
  plugins: [
    tanstackRouter({ target: "react", autoCodeSplitting: true }),
    react(),
    tailwindcss(),
    ...(mock ? [mockApi()] : []),
  ],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
  },
  worker: { format: "es" },
  server: {
    port: 5173,
    // `make dev` runs the Go server on :8080; the UI calls it through this proxy.
    proxy: mock
      ? undefined
      : {
          "/api": "http://localhost:8080",
          "/auth/oidc": "http://localhost:8080",
          "/auth/register": "http://localhost:8080",
          "/auth/invite": {
            target: "http://localhost:8080",
            bypass: (req) => (req.method === "GET" ? req.url : undefined),
          },
          "/auth/reset": {
            target: "http://localhost:8080",
            bypass: (req) => (req.method === "GET" ? req.url : undefined),
          },
          "/datasets-server": "http://localhost:8080",
          "^/.+/resolve/.+": "http://localhost:8080",
          "/healthz": "http://localhost:8080",
        },
  },
  preview: { port: 4173, strictPort: true },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    include: ["src/**/*.test.{ts,tsx}", "mock/**/*.test.ts"],
  },
});
