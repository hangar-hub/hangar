import { PageShell } from "@/components/PageShell";
import { WebhookManager } from "../webhooks/WebhookManager";

/** Every webhook on the Instance (EVT-1, OPS-9). */
export function AdminWebhooksPage() {
  return (
    <PageShell
      title="Webhooks"
      subtitle="Webhooks for every Namespace and Repo. Owners also manage their own from Repo settings."
    >
      <WebhookManager scope={{}} title="All webhooks" />
    </PageShell>
  );
}
