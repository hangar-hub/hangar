import { createFileRoute } from "@tanstack/react-router";
import { RepoListPage } from "@/features/arsenal/RepoListPage";

export const Route = createFileRoute("/models")({
  component: () => <RepoListPage kind="model" />,
});
