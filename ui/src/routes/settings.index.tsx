import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/settings/")({
  component: () => <PageShell title="Settings" />,
});
