import { createFileRoute } from "@tanstack/react-router";
import { AdminOrganisationsPage } from "@/features/admin/AdminOrganisationsPage";

export const Route = createFileRoute("/admin/organisations")({
  component: () => <AdminOrganisationsPage />,
});
