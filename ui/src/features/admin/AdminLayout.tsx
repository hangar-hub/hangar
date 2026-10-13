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
