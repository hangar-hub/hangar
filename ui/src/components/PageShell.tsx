import type { ReactNode } from "react";

/** The frame of a page: a heading, an optional subtitle and the content. */
export function PageShell({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children?: ReactNode;
}) {
  return (
    <section aria-labelledby="page-title" className="space-y-4">
      <header>
        <h1 id="page-title" className="text-2xl font-semibold">
          {title}
        </h1>
        {subtitle ? <p className="font-mono text-sm text-neutral-500">{subtitle}</p> : null}
      </header>
      {children}
    </section>
  );
}
