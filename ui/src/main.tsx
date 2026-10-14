import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import { setNonce } from "get-nonce";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ToastProvider } from "./components/ui/toast";
import { errorStatus } from "./lib/problem";
import { parseSearch, stringifySearch } from "./lib/search-params";
import { applyTheme, loadTheme, resolveTheme } from "./lib/theme";
import { routeTree } from "./routeTree.gen";
import "./styles.css";

// internal/web puts a per-response style nonce in a meta element. Dialogs
// add a style element to lock page scrolling, and the Content Security
// Policy only allows style elements that carry this nonce.
const nonce = document.querySelector<HTMLMetaElement>('meta[name="csp-nonce"]')?.content;
if (nonce) setNonce(nonce);

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      // Retrying a refusal or a missing Repo only delays the message.
      retry: (count, err) => count < 1 && ![401, 403, 404].includes(errorStatus(err)),
    },
  },
});

const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: "intent",
  parseSearch,
  stringifySearch,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

const media = window.matchMedia("(prefers-color-scheme: dark)");
applyTheme(resolveTheme(loadTheme(), media.matches));
media.addEventListener("change", (e) => applyTheme(resolveTheme(loadTheme(), e.matches)));

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
}
