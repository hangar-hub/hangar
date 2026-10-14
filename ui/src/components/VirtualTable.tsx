import { useVirtualizer } from "@tanstack/react-virtual";
import { type ReactNode, useRef } from "react";
import { cn } from "@/lib/utils";
import { virtualiseOver } from "./VirtualGrid";

/**
 * A table of many rows that renders only the rows in view once there are
 * more than 200 (09-web-ui.md "Performance"). It uses ARIA table roles with
 * row counts and indexes, so screen readers know the full size.
 */
export function VirtualTable<T>({
  rows,
  columns,
  label,
  rowHeight = 32,
  template,
}: {
  rows: T[];
  columns: { header: string; cell: (row: T) => ReactNode; className?: string }[];
  label: string;
  rowHeight?: number;
  /** CSS grid template for the columns. */
  template: string;
}) {
  const scroller = useRef<HTMLDivElement>(null);
  const virtual = rows.length > virtualiseOver;
  const v = useVirtualizer({
    count: virtual ? rows.length : 0,
    getScrollElement: () => scroller.current,
    estimateSize: () => rowHeight,
    overscan: 12,
  });
  const gridStyle = { gridTemplateColumns: template };
  const renderRow = (row: T, index: number, style?: React.CSSProperties) => (
    <div
      key={index}
      role="row"
      aria-rowindex={index + 2}
      className="grid items-center border-b border-line px-3 text-[13px] last:border-0"
      style={{ ...gridStyle, ...style, minHeight: rowHeight }}
    >
      {columns.map((c) => (
        <div key={c.header} role="cell" className={cn("min-w-0 truncate py-1 pr-3", c.className)}>
          {c.cell(row)}
        </div>
      ))}
    </div>
  );
  return (
    <div
      role="table"
      aria-label={label}
      aria-rowcount={rows.length + 1}
      // Focusable so keyboard users can scroll it.
      // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrollable table must take focus
      tabIndex={0}
      className="overflow-x-auto rounded-lg border border-line bg-surface"
    >
      <div className="min-w-[560px]">
        <div
          role="row"
          aria-rowindex={1}
          className="grid border-b border-line px-3 py-1.5 text-xs text-muted"
          style={gridStyle}
        >
          {columns.map((c) => (
            <div key={c.header} role="columnheader" className={cn("pr-3 font-medium", c.className)}>
              {c.header}
            </div>
          ))}
        </div>
        {virtual ? (
          <div
            ref={scroller}
            className="max-h-[60vh] overflow-y-auto"
            role="rowgroup"
            // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrollable table must take focus
            tabIndex={0}
          >
            <div className="relative" style={{ height: v.getTotalSize() }}>
              {v.getVirtualItems().map((item) =>
                renderRow(rows[item.index] as T, item.index, {
                  position: "absolute",
                  left: 0,
                  right: 0,
                  top: 0,
                  transform: `translateY(${item.start}px)`,
                }),
              )}
            </div>
          </div>
        ) : (
          <div role="rowgroup">{rows.map((r, i) => renderRow(r, i))}</div>
        )}
      </div>
    </div>
  );
}
