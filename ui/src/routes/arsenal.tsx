import { createFileRoute, redirect } from "@tanstack/react-router";
import { arsenalRedirect } from "@/features/arsenal/filters";

// The Arsenal list became separate Models and Datasets lists on
// 2026-10-16 (docs/build/decisions.md). Old links keep working: they go
// to the list for their type, with their other filters.
export const Route = createFileRoute("/arsenal")({
  beforeLoad: ({ search }) => {
    const target = arsenalRedirect(search as Record<string, unknown>);
    throw redirect({ to: target.to, search: target.search as never, replace: true });
  },
});
