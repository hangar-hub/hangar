import { createFileRoute } from "@tanstack/react-router";
import { AdminSystemPage } from "@/features/admin/AdminSystemPage";

export const Route = createFileRoute("/admin/system")({
  component: () => <AdminSystemPage />,
});
