import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/new")({
  component: () => <PageShell title="New model" />,
});
