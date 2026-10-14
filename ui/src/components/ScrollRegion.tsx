import type { ReactNode, Ref } from "react";
import { cn } from "@/lib/utils";

/**
 * A region that scrolls, and takes keyboard focus so people who do not use
 * a pointer can scroll it with the arrow keys (WCAG 2.1.1, axe
 * scrollable-region-focusable).
 */
export function ScrollRegion({
  label,
  className,
  children,
  ref,
}: {
  label: string;
  className?: string;
  children: ReactNode;
  ref?: Ref<HTMLDivElement>;
}) {
  return (
    // biome-ignore lint/a11y/useSemanticElements: a section would add a landmark for every code block
    <div
      ref={ref}
      role="region"
      aria-label={label}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable regions must take focus
      tabIndex={0}
      className={cn("overflow-auto", className)}
    >
      {children}
    </div>
  );
}
