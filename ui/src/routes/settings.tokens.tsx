import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/settings/tokens")({
  component: () => <PageShell title="Tokens" />,
});
