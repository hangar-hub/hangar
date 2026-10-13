/// <reference types="vitest/config" />

import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The UI builds into internal/web/dist, which the Go binary embeds.
export default defineConfig({
  plugins: [tanstackRouter({ target: "react", autoCodeSplitting: true }), react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
  },
  server: {
    port: 5173,
    // `make dev` runs the Go server on :8080; the UI calls it through this proxy.
    proxy: {
      "/api": "http://localhost:8080",
      "/auth/oidc": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
