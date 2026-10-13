import { type ComponentProps, lazy, Suspense } from "react";
import { Skeleton } from "@/components/ui/states";

const Markdown = lazy(() => import("./Markdown"));

/** Markdown, loaded on first use so the Markdown libraries stay out of the first page load. */
export function LazyMarkdown(props: ComponentProps<typeof Markdown>) {
  return (
    <Suspense fallback={<Skeleton className="h-32 w-full" />}>
      <Markdown {...props} />
    </Suspense>
  );
}
