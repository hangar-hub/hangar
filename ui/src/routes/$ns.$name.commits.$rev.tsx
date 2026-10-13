import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/$ns/$name/commits/$rev")({
  component: Page,
});

function Page() {
  const { ns, name, rev } = Route.useParams();
  return <RepoPageShell kind="model" ns={ns} name={name} page="Commits" detail={rev} />;
}
