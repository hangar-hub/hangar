import { createFileRoute } from "@tanstack/react-router";
import { SessionsPage } from "@/features/settings/SessionsPage";

export const Route = createFileRoute("/settings/sessions")({
  component: () => <SessionsPage />,
});
