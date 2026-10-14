import { createFileRoute } from "@tanstack/react-router";
import { NewRepoPage } from "@/features/repos/NewRepoPage";

export const Route = createFileRoute("/new")({
  component: () => <NewRepoPage kind="model" />,
});
