import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/datasets/$ns/$name/viewer/$config/$split")({
  component: Page,
});

function Page() {
  const { ns, name, config, split } = Route.useParams();
  return (
    <RepoPageShell
      kind="dataset"
      ns={ns}
      name={name}
      page="Viewer"
      detail={`${config} / ${split}`}
    />
  );
}
