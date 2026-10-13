import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/system")({
  component: () => <PageShell title="System" />,
});
