import { createFileRoute } from "@tanstack/react-router";
import { CardPage } from "@/features/repos/CardPage";

export const Route = createFileRoute("/$ns/$name/")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <CardPage kind="model" ns={ns} name={name} />;
}
