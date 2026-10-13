import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/$ns/")({
  component: NamespacePage,
});

function NamespacePage() {
  const { ns } = Route.useParams();
  return <PageShell title={ns} />;
}
