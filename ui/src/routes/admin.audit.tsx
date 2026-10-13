import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/audit")({
  component: () => <PageShell title="Audit log" />,
});
