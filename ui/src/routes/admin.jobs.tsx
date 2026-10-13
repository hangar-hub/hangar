import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/jobs")({
  component: () => <PageShell title="Jobs" />,
});
