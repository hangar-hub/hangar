import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/new-dataset")({
  component: () => <PageShell title="New dataset" />,
});
