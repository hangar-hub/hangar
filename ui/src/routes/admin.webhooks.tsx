import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/webhooks")({
  component: () => <PageShell title="Webhooks" />,
});
