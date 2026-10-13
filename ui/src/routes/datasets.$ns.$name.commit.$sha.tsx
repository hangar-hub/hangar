import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/datasets/$ns/$name/commit/$sha")({
  component: Page,
});

function Page() {
  const { ns, name, sha } = Route.useParams();
  return <RepoPageShell kind="dataset" ns={ns} name={name} page="Commit" detail={`${sha}`} />;
}
