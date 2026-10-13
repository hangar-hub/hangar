import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/datasets/$ns/$name/blob/$rev/$")({
  component: Page,
});

function Page() {
  const { ns, name, rev, _splat } = Route.useParams();
  return (
    <RepoPageShell
      kind="dataset"
      ns={ns}
      name={name}
      page="File"
      detail={`${rev}${_splat ? `/${_splat}` : ""}`}
    />
  );
}
