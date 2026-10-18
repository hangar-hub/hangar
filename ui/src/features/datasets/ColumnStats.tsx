import { BarChart3 } from "lucide-react";
import { Tooltip } from "radix-ui";
import type { ColumnStatistics } from "@/api/viewer";
import { formatCount } from "@/lib/format";

function fmt(n: number | undefined): string {
  if (n === undefined) return "Unknown";
  return Number.isInteger(n) ? formatCount(n) : n.toFixed(3);
}

/** A small histogram or top-values list for a column header. */
export function StatsSummary({ stats }: { stats: ColumnStatistics }) {
  const s = stats.column_statistics;
  if (s.frequencies) {
    const top = Object.entries(s.frequencies)
      .sort((a, b) => b[1] - a[1])
      .slice(0, 8);
    const max = Math.max(...top.map(([, n]) => n), 1);
    return (
      <div className="w-56 space-y-1">
        <p className="text-xs text-muted">
          {s.n_unique !== undefined ? `${formatCount(s.n_unique)} values` : "Top values"}
        </p>
        {top.map(([k, n]) => (
          <div key={k} className="text-xs">
            <div className="flex justify-between gap-2">
              <span className="truncate" title={k}>
                {k}
              </span>
              <span className="tabular font-mono">{formatCount(n)}</span>
            </div>
            <div className="h-1 rounded-full bg-raised">
              <div
                className="h-1 rounded-full bg-accent-fill"
                style={{ width: `${(n / max) * 100}%` }}
              />
            </div>
          </div>
        ))}
      </div>
    );
  }
  const h = s.histogram;
  const max = h ? Math.max(...h.hist, 1) : 1;
  return (
    <div className="w-56 space-y-1.5">
      <p className="text-xs text-muted">
        {stats.column_type === "string_text"
          ? "Text length"
          : stats.column_type === "image"
            ? "Image width"
            : "Values"}
      </p>
      {h ? (
        <div className="flex h-12 items-end gap-px" aria-hidden="true">
          {h.hist.map((n, i) => (
            <div
              key={`${h.bin_edges[i]}-${h.bin_edges[i + 1]}`}
              className="flex-1 rounded-t-sm bg-accent-fill"
              style={{ height: `${Math.max(4, (n / max) * 100)}%` }}
            />
          ))}
        </div>
      ) : null}
      <dl className="tabular grid grid-cols-2 gap-x-2 font-mono text-[11px]">
        <dt className="text-muted">min</dt>
        <dd>{fmt(s.min)}</dd>
        <dt className="text-muted">max</dt>
        <dd>{fmt(s.max)}</dd>
        <dt className="text-muted">mean</dt>
        <dd>{fmt(s.mean)}</dd>
        <dt className="text-muted">median</dt>
        <dd>{fmt(s.median)}</dd>
        {s.nan_count ? (
          <>
            <dt className="text-muted">null</dt>
            <dd>{formatCount(s.nan_count)}</dd>
          </>
        ) : null}
      </dl>
    </div>
  );
}

/** A statistics button in a column header; the summary shows on hover and on keyboard focus. */
export function ColumnStatsButton({ stats, column }: { stats?: ColumnStatistics; column: string }) {
  if (!stats || !Object.keys(stats.column_statistics).length) return null;
  return (
    <Tooltip.Provider delayDuration={150}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <button
            type="button"
            aria-label={`Statistics for ${column}`}
            className="rounded-sm p-0.5 text-muted transition-colors duration-100 hover:bg-raised hover:text-ink"
          >
            <BarChart3 className="size-3.5" aria-hidden="true" />
          </button>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            side="bottom"
            sideOffset={6}
            className="hg-popover z-50 rounded-md border border-line bg-surface p-3 text-ink shadow-lg"
          >
            <StatsSummary stats={stats} />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  );
}
