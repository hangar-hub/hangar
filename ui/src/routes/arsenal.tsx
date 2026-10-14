import { createFileRoute } from "@tanstack/react-router";
import { ArsenalPage } from "@/features/arsenal/ArsenalPage";

export const Route = createFileRoute("/arsenal")({
  component: () => <ArsenalPage />,
});
