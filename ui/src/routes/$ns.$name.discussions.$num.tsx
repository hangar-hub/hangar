import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/$ns/$name/discussions/$num")({
  component: Page,
});

function Page() {
  const { ns, name, num } = Route.useParams();
  return <RepoPageShell kind="model" ns={ns} name={name} page="Discussion" detail={`#${num}`} />;
}
