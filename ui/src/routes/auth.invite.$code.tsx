import { createFileRoute } from "@tanstack/react-router";
import { AccountLinkPage } from "@/features/auth/AccountLinkPage";

export const Route = createFileRoute("/auth/invite/$code")({
  component: Page,
});

function Page() {
  const { code } = Route.useParams();
  return <AccountLinkPage mode="invite" code={code} />;
}
