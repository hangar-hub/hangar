import { createFileRoute } from "@tanstack/react-router";
import { ProfilePage } from "@/features/settings/ProfilePage";

export const Route = createFileRoute("/settings/profile")({
  component: () => <ProfilePage />,
});
