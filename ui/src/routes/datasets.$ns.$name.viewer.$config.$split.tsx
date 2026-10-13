import { createFileRoute } from "@tanstack/react-router";
import { ViewerPage } from "@/features/datasets/ViewerPage";

export const Route = createFileRoute("/datasets/$ns/$name/viewer/$config/$split")({
  component: Page,
});

function Page() {
  const { ns, name, config, split } = Route.useParams();
  return <ViewerPage ns={ns} name={name} config={config} split={split} />;
}
