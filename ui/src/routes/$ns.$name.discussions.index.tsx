import { createFileRoute } from "@tanstack/react-router";
import { DiscussionsPage } from "@/features/community/DiscussionsPage";

export const Route = createFileRoute("/$ns/$name/discussions/")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <DiscussionsPage kind="model" ns={ns} name={name} />;
}
