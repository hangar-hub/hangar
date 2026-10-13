import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/auth/reset/$code")({
  component: ResetPage,
});

function ResetPage() {
  return <PageShell title="Reset your password" />;
}
