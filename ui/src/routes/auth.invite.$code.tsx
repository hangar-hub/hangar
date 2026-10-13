import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/auth/invite/$code")({
  component: InvitePage,
});

function InvitePage() {
  return <PageShell title="Accept your invite" />;
}
