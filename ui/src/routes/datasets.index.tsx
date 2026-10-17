import { createFileRoute } from "@tanstack/react-router";
import { RepoListPage } from "@/features/arsenal/RepoListPage";

export const Route = createFileRoute("/datasets/")({
  component: () => <RepoListPage kind="dataset" />,
});
