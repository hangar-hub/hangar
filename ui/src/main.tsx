import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { applyTheme, loadTheme, resolveTheme } from "./lib/theme";
import { routeTree } from "./routeTree.gen";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 30_000, retry: 1 } },
});

const router = createRouter({ routeTree, context: { queryClient }, defaultPreload: "intent" });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

const prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
applyTheme(resolveTheme(loadTheme(), prefersDark));

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </StrictMode>,
  );
}
