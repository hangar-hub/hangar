import type { QueryClient } from "@tanstack/react-query";
import { createRootRouteWithContext, Link, Outlet } from "@tanstack/react-router";
import { AppShell } from "@/components/AppShell";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { ErrorState } from "@/components/ui/states";

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: () => (
    <AppShell>
      <Outlet />
    </AppShell>
  ),
  notFoundComponent: () => (
    <PageShell title="Page not found">
      <p className="text-sm text-muted">Check the address, or go back to your models.</p>
      <Button asChild variant="primary">
        <Link to="/models">Open your models</Link>
      </Button>
    </PageShell>
  ),
  errorComponent: ({ error, reset }) => (
    <AppShell>
      <PageShell title="Something went wrong">
        <ErrorState error={error} onRetry={reset} />
      </PageShell>
    </AppShell>
  ),
});
