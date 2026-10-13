import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/settings/upstream-tokens")({
  component: () => <PageShell title="Upstream tokens" />,
});
