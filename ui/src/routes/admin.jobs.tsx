import { createFileRoute } from "@tanstack/react-router";
import { AdminJobsPage } from "@/features/admin/AdminJobsPage";

export const Route = createFileRoute("/admin/jobs")({
  component: () => <AdminJobsPage />,
});
