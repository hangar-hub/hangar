import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/discover")({
  component: () => <PageShell title="Discover" />,
});
