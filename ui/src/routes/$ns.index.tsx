import { createFileRoute } from "@tanstack/react-router";
import { NamespacePage } from "@/features/NamespacePage";

export const Route = createFileRoute("/$ns/")({
  component: Page,
});

function Page() {
  const { ns } = Route.useParams();
  return <NamespacePage ns={ns} />;
}
