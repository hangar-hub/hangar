import type { QueryClient } from "@tanstack/react-query";
import { createRootRouteWithContext, Outlet } from "@tanstack/react-router";
import { AppShell } from "@/components/AppShell";
import { PageShell } from "@/components/PageShell";

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: () => (
    <AppShell>
      <Outlet />
    </AppShell>
  ),
  notFoundComponent: () => (
    <PageShell title="Page not found">
      <p>Check the address, or go back to your arsenal.</p>
    </PageShell>
  ),
});
