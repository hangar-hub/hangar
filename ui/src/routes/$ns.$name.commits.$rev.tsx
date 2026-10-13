import { createFileRoute } from "@tanstack/react-router";
import { CommitsPage } from "@/features/files/CommitsPage";

export const Route = createFileRoute("/$ns/$name/commits/$rev")({
  component: Page,
});

function Page() {
  const { ns, name, rev } = Route.useParams();
  return <CommitsPage kind="model" ns={ns} name={name} rev={rev} />;
}
