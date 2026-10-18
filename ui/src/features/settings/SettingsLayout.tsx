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

// A bar on the left marks the current page on wide screens; on phones the
// nav scrolls sideways and the filled background marks it instead.
const sideNavItem =
  "relative whitespace-nowrap rounded-md px-3 py-1.5 text-sm text-muted transition-[color,background-color] duration-150 hover:bg-raised hover:text-ink md:before:absolute md:before:inset-y-1.5 md:before:left-0 md:before:w-0.5 md:before:rounded-full md:before:transition-[background-color] md:before:duration-150";
const sideNavActive = "bg-raised font-medium !text-ink md:before:bg-accent";

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
            className={sideNavItem}
            activeProps={{ className: sideNavActive, "aria-current": "page" }}
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
