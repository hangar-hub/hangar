import { createFileRoute } from "@tanstack/react-router";
import { CommitPage } from "@/features/files/CommitPage";

export const Route = createFileRoute("/datasets/$ns/$name/commit/$sha")({
  component: Page,
});

function Page() {
  const { ns, name, sha } = Route.useParams();
  return <CommitPage kind="dataset" ns={ns} name={name} sha={sha} />;
}
