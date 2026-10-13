import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/$ns/$name/settings")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <RepoPageShell kind="model" ns={ns} name={name} page="Settings" />;
}
