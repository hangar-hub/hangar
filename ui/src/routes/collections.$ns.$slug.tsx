import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/collections/$ns/$slug")({
  component: CollectionPage,
});

function CollectionPage() {
  const { ns, slug } = Route.useParams();
  return <PageShell title="Collection" subtitle={`${ns}/${slug}`} />;
}
