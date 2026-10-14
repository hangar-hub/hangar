import { createFileRoute } from "@tanstack/react-router";
import { CardPage } from "@/features/repos/CardPage";

export const Route = createFileRoute("/datasets/$ns/$name/")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <CardPage kind="dataset" ns={ns} name={name} />;
}
