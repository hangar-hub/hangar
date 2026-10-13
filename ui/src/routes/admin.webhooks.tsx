import { createFileRoute } from "@tanstack/react-router";
import { AdminWebhooksPage } from "@/features/admin/AdminWebhooksPage";

export const Route = createFileRoute("/admin/webhooks")({
  component: () => <AdminWebhooksPage />,
});
