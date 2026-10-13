import { createFileRoute } from "@tanstack/react-router";
import { TreePage } from "@/features/files/TreePage";

export const Route = createFileRoute("/datasets/$ns/$name/tree/$rev/$")({
  component: Page,
});

function Page() {
  const { ns, name, rev, _splat } = Route.useParams();
  return <TreePage kind="dataset" ns={ns} name={name} rev={rev} path={_splat ?? ""} />;
}
