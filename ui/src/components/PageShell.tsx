import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** The frame of a page: a heading, an optional subtitle and actions, then the content. */
export function PageShell({
  title,
  subtitle,
  actions,
  children,
  className,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  children?: ReactNode;
  /** Extra classes for the page's section, such as `hg-enter` once its data loads. */
  className?: string;
}) {
  return (
    <section aria-labelledby="page-title" className={cn("space-y-5", className)}>
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div className="min-w-0">
          <h1 id="page-title" className="text-2xl font-semibold tracking-tight">
            {title}
          </h1>
          {subtitle ? <p className="mt-1 text-sm text-muted">{subtitle}</p> : null}
        </div>
        {actions ? <div className="flex flex-wrap gap-2">{actions}</div> : null}
      </header>
      {children}
    </section>
  );
}
