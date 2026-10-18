import { Link, Outlet } from "@tanstack/react-router";
import { PermissionDenied, Skeleton } from "@/components/ui/states";
import { useMe } from "@/lib/session";

const items = [
  { to: "/admin/system", label: "System" },
  { to: "/admin/users", label: "Users" },
  { to: "/admin/organisations", label: "Organisations" },
  { to: "/admin/upstreams", label: "Upstreams" },
  { to: "/admin/pulls", label: "Pull settings" },
  { to: "/admin/storage", label: "Storage" },
  { to: "/admin/jobs", label: "Jobs" },
  { to: "/admin/webhooks", label: "Webhooks" },
  { to: "/admin/audit", label: "Audit log" },
  { to: "/admin/settings", label: "Instance settings" },
] as const;

// A bar on the left marks the current page on wide screens; on phones the
// nav scrolls sideways and the filled background marks it instead.
const sideNavItem =
  "relative whitespace-nowrap rounded-md px-3 py-1.5 text-sm text-muted transition-[color,background-color] duration-150 hover:bg-raised hover:text-ink md:before:absolute md:before:inset-y-1.5 md:before:left-0 md:before:w-0.5 md:before:rounded-full md:before:transition-[background-color] md:before:duration-150";
const sideNavActive = "bg-raised font-medium !text-ink md:before:bg-accent";

/** `/admin/...`: Instance administration for Instance admins (OPS-9). */
export function AdminLayout() {
  const me = useMe();
  if (me.isPending) return <Skeleton className="h-64" />;
  if (!me.data?.is_admin)
    return <PermissionDenied>Only Instance admins can open the admin pages.</PermissionDenied>;
  return (
    <div className="grid gap-6 md:grid-cols-[200px_minmax(0,1fr)]">
      <nav aria-label="Admin" className="flex gap-1 overflow-x-auto md:flex-col">
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
