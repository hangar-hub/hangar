import { createFileRoute } from "@tanstack/react-router";
import { RepoPageShell } from "@/pages/repo";

export const Route = createFileRoute("/$ns/$name/blob/$rev/$")({
  component: Page,
});

function Page() {
  const { ns, name, rev, _splat } = Route.useParams();
  return (
    <RepoPageShell
      kind="model"
      ns={ns}
      name={name}
      page="File"
      detail={`${rev}${_splat ? `/${_splat}` : ""}`}
    />
  );
}
