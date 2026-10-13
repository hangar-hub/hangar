import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/settings/sessions")({
  component: () => <PageShell title="Sessions" />,
});
