import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/collections/")({
  component: () => <PageShell title="Collections" />,
});
