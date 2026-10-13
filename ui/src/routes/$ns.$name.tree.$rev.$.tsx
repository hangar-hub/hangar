import { createFileRoute } from "@tanstack/react-router";
import { TreePage } from "@/features/files/TreePage";

export const Route = createFileRoute("/$ns/$name/tree/$rev/$")({
  component: Page,
});

function Page() {
  const { ns, name, rev, _splat } = Route.useParams();
  return <TreePage kind="model" ns={ns} name={name} rev={rev} path={_splat ?? ""} />;
}
