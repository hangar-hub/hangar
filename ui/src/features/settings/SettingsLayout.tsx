import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { EmptyState, Skeleton } from "@/components/ui/states";
import { useMe } from "@/lib/session";

const items = [
  { to: "/settings/profile", label: "Profile" },
  { to: "/settings/tokens", label: "Tokens" },
  { to: "/settings/ollama-keys", label: "Ollama keys" },
  { to: "/settings/upstream-tokens", label: "Upstream tokens" },
  { to: "/settings/sessions", label: "Sessions" },
  { to: "/settings/organisations", label: "Organisations" },
] as const;

/** `/settings/...`: the signed-in user's own settings. */
export function SettingsLayout() {
  const me = useMe();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  if (me.isPending) return <Skeleton className="h-64" />;
  if (!me.data) {
    return (
      <EmptyState
        title="Sign in to see your settings"
        action={
          <Button asChild variant="primary">
            <Link to="/auth/login" search={{ redirect: pathname }}>
              Sign in
            </Link>
          </Button>
        }
      />
    );
  }
  return (
    <div className="grid gap-6 md:grid-cols-[200px_minmax(0,1fr)]">
      <nav aria-label="Settings" className="flex gap-1 overflow-x-auto md:flex-col">
        {items.map((i) => (
          <Link
            key={i.to}
            to={i.to}
            className="whitespace-nowrap rounded-md px-3 py-1.5 text-sm text-muted hover:bg-raised hover:text-ink"
            activeProps={{ className: "bg-raised font-medium !text-ink", "aria-current": "page" }}
          >
            {i.label}
          </Link>
        ))}
      </nav>
      <div className="min-w-0">
        <Outlet />
      </div>
    </div>
  );
}
