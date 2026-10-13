import { createFileRoute } from "@tanstack/react-router";
import { PageShell } from "@/components/PageShell";

export const Route = createFileRoute("/settings/ollama-keys")({
  component: () => <PageShell title="Ollama keys" />,
});
