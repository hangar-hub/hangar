import { createFileRoute } from "@tanstack/react-router";
import { AdminStoragePage } from "@/features/admin/AdminStoragePage";

export const Route = createFileRoute("/admin/storage")({
  component: () => <AdminStoragePage />,
});
