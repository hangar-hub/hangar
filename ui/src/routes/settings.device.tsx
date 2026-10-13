import { createFileRoute } from "@tanstack/react-router";
import { DevicePage } from "@/features/settings/DevicePage";

export const Route = createFileRoute("/settings/device")({
  component: () => <DevicePage />,
});
