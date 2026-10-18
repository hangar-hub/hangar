import { createFileRoute } from "@tanstack/react-router";
import { EmailConfirmPage } from "@/features/auth/EmailConfirmPage";

export const Route = createFileRoute("/auth/email/$code")({
  component: Page,
});

function Page() {
  const { code } = Route.useParams();
  return <EmailConfirmPage code={code} />;
}
