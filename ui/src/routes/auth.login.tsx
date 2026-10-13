import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/auth/login")({
  component: () => <PageShell title="Sign in to Hangar" />,
});
