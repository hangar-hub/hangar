import { createFileRoute } from "@tanstack/react-router";
import { AdminPullsPage } from "@/features/admin/AdminPullsPage";

export const Route = createFileRoute("/admin/pulls")({
  component: () => <AdminPullsPage />,
});
