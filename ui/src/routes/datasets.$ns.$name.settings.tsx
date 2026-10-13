import { createFileRoute } from "@tanstack/react-router";
import { RepoSettingsPage } from "@/features/repos/RepoSettingsPage";

export const Route = createFileRoute("/datasets/$ns/$name/settings")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <RepoSettingsPage kind="dataset" ns={ns} name={name} />;
}
