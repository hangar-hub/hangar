import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/datasets/$ns/$name/commits/$rev")({
  component: Page,
});

function Page() {
  const { ns, name, rev } = Route.useParams();
  return <RepoPageShell kind="dataset" ns={ns} name={name} page="Commits" detail={rev} />;
}
