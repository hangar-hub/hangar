import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/datasets/$ns/$name/discussions/$num")({
  component: Page,
});

function Page() {
  const { ns, name, num } = Route.useParams();
  return <RepoPageShell kind="dataset" ns={ns} name={name} page="Discussion" detail={`#${num}`} />;
}
