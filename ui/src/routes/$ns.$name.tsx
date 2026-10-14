import { createFileRoute } from "@tanstack/react-router";
import { RepoLayout } from "@/features/repos/RepoLayout";

export const Route = createFileRoute("/$ns/$name")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <RepoLayout kind="model" ns={ns} name={name} />;
}
