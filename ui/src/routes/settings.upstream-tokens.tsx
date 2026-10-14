import { createFileRoute } from "@tanstack/react-router";
import { UpstreamTokensPage } from "@/features/settings/UpstreamTokensPage";

export const Route = createFileRoute("/settings/upstream-tokens")({
  component: () => <UpstreamTokensPage />,
});
