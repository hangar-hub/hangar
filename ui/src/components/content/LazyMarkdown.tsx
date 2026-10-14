import { type ComponentProps, lazy, Suspense } from "react";
import { Skeleton } from "@/components/ui/states";

const Markdown = lazy(() => import("./Markdown"));

/**
 * Markdown, loaded on first use so the Markdown libraries stay out of the
 * first page load. While the code loads, the placeholder is a status that
 * says so, like every other loading state.
 */
export function LazyMarkdown(props: ComponentProps<typeof Markdown>) {
  return (
    <Suspense
      fallback={
        <div role="status" aria-label="Loading Markdown">
          <Skeleton className="h-32 w-full" />
        </div>
      }
    >
      <Markdown {...props} />
    </Suspense>
  );
}
