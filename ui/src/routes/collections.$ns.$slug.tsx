import { createFileRoute } from "@tanstack/react-router";
import { CollectionPage } from "@/features/collections/CollectionPage";

export const Route = createFileRoute("/collections/$ns/$slug")({
  component: Page,
});

function Page() {
  const { ns, slug } = Route.useParams();
  return <CollectionPage ns={ns} slug={slug} />;
}
