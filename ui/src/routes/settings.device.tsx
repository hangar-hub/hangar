import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/settings/device")({
  component: () => <PageShell title="Approve a device sign-in" />,
});
