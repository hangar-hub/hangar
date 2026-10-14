import { createFileRoute } from "@tanstack/react-router";
import { DiscussionPage } from "@/features/community/DiscussionPage";

export const Route = createFileRoute("/datasets/$ns/$name/discussions/$num")({
  component: Page,
});

function Page() {
  const { ns, name, num } = Route.useParams();
  return <DiscussionPage kind="dataset" ns={ns} name={name} num={Number(num)} />;
}
