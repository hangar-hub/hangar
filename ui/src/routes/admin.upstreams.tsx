import { createFileRoute } from "@tanstack/react-router";
import { AdminUpstreamsPage } from "@/features/admin/AdminUpstreamsPage";

export const Route = createFileRoute("/admin/upstreams")({
  component: () => <AdminUpstreamsPage />,
});
