import { createFileRoute } from "@tanstack/react-router";
import { OllamaKeysPage } from "@/features/settings/OllamaKeysPage";

export const Route = createFileRoute("/settings/ollama-keys")({
  component: () => <OllamaKeysPage />,
});
