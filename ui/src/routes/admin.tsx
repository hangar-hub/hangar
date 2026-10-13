import { createFileRoute } from "@tanstack/react-router";
import { AdminLayout } from "@/features/admin/AdminLayout";

export const Route = createFileRoute("/admin")({
  component: AdminLayout,
});
