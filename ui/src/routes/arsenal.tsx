import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/arsenal")({
  component: () => <PageShell title="Arsenal" />,
});
