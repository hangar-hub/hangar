import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export type Column<T> = {
  key: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  className?: string;
  /** Hidden from the card layout on phones. */
  hideOnCard?: boolean;
  /** The column that heads each card on phones. */
  primary?: boolean;
};

/**
 * A table that becomes a list of cards on narrow screens (09-web-ui.md
 * "Responsive"). It keeps table semantics on every width.
 */
export function DataTable<T>({
  columns,
  rows,
  rowKey,
  caption,
  className,
}: {
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  caption: string;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border border-line bg-surface max-md:border-0 max-md:bg-transparent",
        className,
      )}
    >
      <table className="w-full border-collapse text-sm max-md:block">
        <caption className="sr-only">{caption}</caption>
        <thead className="max-md:sr-only">
          <tr className="border-b border-line text-left text-xs text-muted">
            {columns.map((c) => (
              <th key={c.key} scope="col" className={cn("px-3 py-2 font-medium", c.className)}>
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="max-md:block max-md:space-y-2">
          {rows.map((row) => (
            <tr
              key={rowKey(row)}
              className="border-b border-line last:border-0 max-md:block max-md:rounded-lg max-md:border max-md:bg-surface max-md:p-3 max-md:last:border"
            >
              {columns.map((c) => (
                <td
                  key={c.key}
                  className={cn(
                    "px-3 py-2 align-middle max-md:flex max-md:items-baseline max-md:justify-between max-md:gap-3 max-md:px-0 max-md:py-0.5",
                    c.primary && "max-md:block max-md:pb-1.5",
                    c.hideOnCard && "max-md:hidden",
                    c.className,
                  )}
                >
                  {!c.primary ? (
                    <span className="shrink-0 text-xs text-muted md:hidden" aria-hidden="true">
                      {c.header}
                    </span>
                  ) : null}
                  <span className={cn("min-w-0", !c.primary && "max-md:text-right")}>
                    {c.cell(row)}
                  </span>
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
