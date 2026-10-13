import { createFileRoute } from "@tanstack/react-router";
import { DiscussionsPage } from "@/features/community/DiscussionsPage";

export const Route = createFileRoute("/datasets/$ns/$name/discussions/")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <DiscussionsPage kind="dataset" ns={ns} name={name} />;
}
