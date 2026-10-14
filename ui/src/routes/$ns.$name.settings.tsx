import { createFileRoute } from "@tanstack/react-router";
import { RepoSettingsPage } from "@/features/repos/RepoSettingsPage";

export const Route = createFileRoute("/$ns/$name/settings")({
  component: Page,
});

function Page() {
  const { ns, name } = Route.useParams();
  return <RepoSettingsPage kind="model" ns={ns} name={name} />;
}
