import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/admin/upstreams")({
  component: () => <PageShell title="Upstreams" />,
});
