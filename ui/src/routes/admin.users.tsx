import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/users")({
  component: () => <PageShell title="Users" />,
});
