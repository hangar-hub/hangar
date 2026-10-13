import { createFileRoute } from "@tanstack/react-router";
import { AccountLinkPage } from "@/features/auth/AccountLinkPage";

export const Route = createFileRoute("/auth/reset/$code")({
  component: Page,
});

function Page() {
  const { code } = Route.useParams();
  return <AccountLinkPage mode="reset" code={code} />;
}
