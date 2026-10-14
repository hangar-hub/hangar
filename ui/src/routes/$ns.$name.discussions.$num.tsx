import { createFileRoute } from "@tanstack/react-router";
import { DiscussionPage } from "@/features/community/DiscussionPage";

export const Route = createFileRoute("/$ns/$name/discussions/$num")({
  component: Page,
});

function Page() {
  const { ns, name, num } = Route.useParams();
  return <DiscussionPage kind="model" ns={ns} name={name} num={Number(num)} />;
}
