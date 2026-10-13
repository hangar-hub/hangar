import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/settings")({
  component: () => <PageShell title="Instance settings" />,
});
