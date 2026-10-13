import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/")({
  component: () => (
    <PageShell title="Hangar" subtitle="A self-hosted home for your arsenal of models." />
  ),
});
