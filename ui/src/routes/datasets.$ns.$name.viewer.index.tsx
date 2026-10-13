import { createFileRoute } from "@tanstack/react-router";
import { ViewerPage } from "@/features/datasets/ViewerPage";

export const Route = createFileRoute("/datasets/$ns/$name/viewer/")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <ViewerPage ns={ns} name={name} />;
}
