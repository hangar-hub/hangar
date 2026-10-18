import { useWindowVirtualizer } from "@tanstack/react-virtual";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";

/** Lists longer than this render only the rows on screen (09-web-ui.md "Performance"). */
export const virtualiseOver = 200;

function useColumns(ref: React.RefObject<HTMLElement | null>, minWidth: number): number {
  const [cols, setCols] = useState(1);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const update = () => setCols(Math.max(1, Math.floor((el.clientWidth + 12) / (minWidth + 12))));
    update();
    const ro = new ResizeObserver(update);
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, minWidth]);
  return cols;
}

/**
 * Renders items in a responsive grid (or a single column), virtualised
 * against the window once there are more than 200, so long lists stay fast.
 * Each item fills its cell, so items in a row are the same height.
 * `onEnd` fires when the last row comes into view, to load the next page.
 * `fadeIn` fades the grid in when it mounts; it changes only opacity, so
 * it cannot upset the virtualiser's measurements.
 */
export function VirtualGrid<T>({
  items,
  render,
  itemKey,
  minColumnWidth = 300,
  estimateRowHeight = 132,
  onEnd,
  label,
  fadeIn,
}: {
  items: T[];
  render: (item: T) => ReactNode;
  itemKey: (item: T) => string;
  minColumnWidth?: number;
  estimateRowHeight?: number;
  onEnd?: () => void;
  label: string;
  fadeIn?: boolean;
}) {
  const ref = useRef<HTMLUListElement>(null);
  const cols = useColumns(ref, minColumnWidth);
  const rows = Math.ceil(items.length / cols);
  const virtual = items.length > virtualiseOver;
  const [offset, setOffset] = useState(0);
  useLayoutEffect(() => {
    setOffset(ref.current ? ref.current.getBoundingClientRect().top + window.scrollY : 0);
  }, []);
  const v = useWindowVirtualizer({
    count: virtual ? rows : 0,
    estimateSize: () => estimateRowHeight,
    overscan: 4,
    scrollMargin: offset,
  });
  const vItems = v.getVirtualItems();
  const last = vItems[vItems.length - 1];
  useEffect(() => {
    if (virtual && onEnd && last && last.index >= rows - 1) onEnd();
  }, [virtual, onEnd, last, rows]);

  const gridStyle = { gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))` };
  if (!virtual) {
    return (
      <ul
        ref={ref}
        aria-label={label}
        className={cn("grid gap-3", fadeIn && "hg-fade-enter")}
        style={gridStyle}
      >
        {items.map((it) => (
          // A one-cell grid, so the item fills the row's height.
          <li key={itemKey(it)} className="grid">
            {render(it)}
          </li>
        ))}
      </ul>
    );
  }
  return (
    <ul
      ref={ref}
      aria-label={label}
      className={cn("relative", fadeIn && "hg-fade-enter")}
      style={{ height: v.getTotalSize() }}
    >
      {vItems.map((row) => (
        <li
          key={row.key}
          data-index={row.index}
          ref={v.measureElement}
          className="absolute inset-x-0 grid gap-3 pb-3"
          style={{ ...gridStyle, transform: `translateY(${row.start - v.options.scrollMargin}px)` }}
        >
          {items.slice(row.index * cols, row.index * cols + cols).map((it) => (
            <div key={itemKey(it)} className="grid">
              {render(it)}
            </div>
          ))}
        </li>
      ))}
    </ul>
  );
}
