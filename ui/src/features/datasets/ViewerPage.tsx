import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ArrowDown, ArrowUp, ArrowUpDown, FileSearch, Filter, Plus, Search, X } from "lucide-react";
import { useRef, useState } from "react";
import { api } from "@/api/client";
import type { DatasetSplit } from "@/api/types";
import {
  type ColumnStatistics,
  type Feature,
  fetchRows,
  fetchSize,
  fetchStatistics,
  isNumericFeature,
  type RowsResponse,
  ViewerError,
} from "@/api/viewer";
import { AppLink, useGo } from "@/components/AppLink";
import { ScrollRegion } from "@/components/ScrollRegion";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Select, Textarea } from "@/components/ui/form";
import { ProgressBar } from "@/components/ui/progress";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { formatBytes, formatCount } from "@/lib/format";
import { errorMessage, errorStatus, unwrap } from "@/lib/problem";
import { repoPath, treePath } from "@/lib/repo";
import { useSearchState } from "@/lib/use-search-state";
import { cn } from "@/lib/utils";
import {
  buildOrderBy,
  buildWhere,
  type Condition,
  needsValue,
  nextSort,
  type Operator,
  operatorLabels,
  operators,
  parseOrderBy,
} from "@/lib/where";
import { useDatasetConfigs, useRepo } from "../repos/hooks";
import { Cell } from "./Cell";
import { ColumnStatsButton } from "./ColumnStats";

const pageSize = 100;

type ViewerSearch = { q?: string; where?: string; orderby?: string; page?: string };

function Preparing({ split }: { split?: DatasetSplit }) {
  const job = useQuery({
    queryKey: ["jobs", split?.job_id],
    enabled: Boolean(split?.job_id),
    queryFn: async () =>
      unwrap(await api.GET("/jobs/{id}", { params: { path: { id: split?.job_id ?? "" } } })),
    refetchInterval: 2000,
  });
  const p = job.data?.progress;
  return (
    <div
      role="status"
      className="space-y-2 rounded-lg border border-info/30 bg-info-soft px-4 py-4"
    >
      <p className="font-medium">
        Preparing preview. Hangar is converting this dataset to Parquet.
      </p>
      {p && p.bytes_total > 0 ? (
        <>
          <ProgressBar done={p.bytes_done} total={p.bytes_total} label="Parquet export progress" />
          <p className="tabular font-mono text-xs text-muted">
            {formatBytes(p.bytes_done)} of {formatBytes(p.bytes_total)}
            {p.message ? `, ${p.message}` : ""}
          </p>
        </>
      ) : (
        <p className="text-sm text-muted">
          The preview appears here when the export finishes. You can load the dataset with the
          datasets library already.
        </p>
      )}
    </div>
  );
}

function FilterBar({
  features,
  where,
  onApply,
}: {
  features: Feature[];
  where: string;
  onApply: (where: string) => void;
}) {
  const [raw, setRaw] = useState(Boolean(where));
  const [rawText, setRawText] = useState(where);
  const [conditions, setConditions] = useState<Condition[]>([
    { column: features[0]?.name ?? "", op: "=", value: "" },
  ]);
  const numeric = (col: string) => isNumericFeature(features.find((f) => f.name === col)?.type);
  const built = buildWhere(conditions, numeric);
  const update = (i: number, patch: Partial<Condition>) =>
    setConditions(conditions.map((c, j) => (j === i ? { ...c, ...patch } : c)));
  return (
    <form
      aria-label="Filter rows"
      className="space-y-2 rounded-lg border border-line bg-surface p-3"
      onSubmit={(e) => {
        e.preventDefault();
        onApply(raw ? rawText.trim() : built);
      }}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">Filter</span>
        <Button
          size="sm"
          variant="ghost"
          aria-pressed={raw}
          onClick={() => {
            if (!raw) setRawText(built || where);
            setRaw(!raw);
          }}
        >
          {raw ? "Use the builder" : "Write SQL"}
        </Button>
      </div>
      {raw ? (
        <Textarea
          aria-label="Where clause"
          rows={2}
          className="font-mono text-xs"
          placeholder={`"label" = 1 AND "text" ILIKE '%great%'`}
          value={rawText}
          onChange={(e) => setRawText(e.target.value)}
        />
      ) : (
        <ul className="space-y-2">
          {conditions.map((c, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: conditions are edited by position
            <li key={i} className="flex flex-wrap items-center gap-2">
              {i > 0 ? <span className="text-xs text-muted">and</span> : null}
              <Select
                aria-label="Column"
                className="w-auto font-mono text-xs"
                value={c.column}
                onChange={(e) => update(i, { column: e.target.value })}
              >
                {features.map((f) => (
                  <option key={f.name} value={f.name}>
                    {f.name}
                  </option>
                ))}
              </Select>
              <Select
                aria-label="Operator"
                className="w-auto text-xs"
                value={c.op}
                onChange={(e) => update(i, { op: e.target.value as Operator })}
              >
                {operators.map((o) => (
                  <option key={o} value={o}>
                    {operatorLabels[o]}
                  </option>
                ))}
              </Select>
              {needsValue(c.op) ? (
                <Input
                  aria-label="Value"
                  className="w-48 font-mono text-xs"
                  placeholder={c.op === "IN" ? "a, b, c" : "Value"}
                  value={c.value}
                  onChange={(e) => update(i, { value: e.target.value })}
                />
              ) : null}
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label="Remove this condition"
                onClick={() => setConditions(conditions.filter((_, j) => j !== i))}
              >
                <X aria-hidden="true" />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {!raw ? (
          <Button
            size="sm"
            variant="ghost"
            onClick={() =>
              setConditions([
                ...conditions,
                { column: features[0]?.name ?? "", op: "=", value: "" },
              ])
            }
          >
            <Plus aria-hidden="true" />
            Add condition
          </Button>
        ) : null}
        {!raw && built ? (
          <code className="min-w-0 break-all font-mono text-xs text-muted">{built}</code>
        ) : null}
        <span className="ml-auto flex gap-2">
          {where ? (
            <Button size="sm" onClick={() => onApply("")}>
              Clear filter
            </Button>
          ) : null}
          <Button size="sm" type="submit" variant="primary">
            Apply filter
          </Button>
        </span>
      </div>
    </form>
  );
}

function RowsTable({
  data,
  stats,
  sort,
  onSort,
}: {
  data: RowsResponse;
  stats?: Record<string, ColumnStatistics>;
  sort: ReturnType<typeof parseOrderBy>;
  onSort: (col: string) => void;
}) {
  const scroller = useRef<HTMLDivElement>(null);
  const v = useVirtualizer({
    count: data.rows.length,
    getScrollElement: () => scroller.current,
    estimateSize: () => 56,
    overscan: 6,
  });
  const template = `72px repeat(${data.features.length}, minmax(180px, 1fr))`;
  return (
    <ScrollRegion
      ref={scroller}
      label="Rows, scrollable"
      className="max-h-[70vh] rounded-lg border border-line bg-surface"
    >
      <div
        role="table"
        aria-label="Rows"
        aria-rowcount={data.num_rows_total + 1}
        style={{ minWidth: `${72 + data.features.length * 180}px` }}
      >
        <div>
          <div
            role="row"
            aria-rowindex={1}
            className="sticky top-0 z-10 grid border-b border-line bg-surface"
            style={{ gridTemplateColumns: template }}
          >
            <div role="columnheader" className="px-3 py-2 text-xs text-muted">
              Row
            </div>
            {data.features.map((f) => {
              const active = sort?.column === f.name ? sort.dir : undefined;
              const Icon = active === "ASC" ? ArrowUp : active === "DESC" ? ArrowDown : ArrowUpDown;
              return (
                <div
                  key={f.name}
                  role="columnheader"
                  aria-sort={
                    active === "ASC" ? "ascending" : active === "DESC" ? "descending" : "none"
                  }
                  className="flex items-center gap-1 px-3 py-2"
                >
                  <button
                    type="button"
                    onClick={() => onSort(f.name)}
                    aria-label={`Sort by ${f.name}`}
                    className="flex min-w-0 items-center gap-1 text-left text-xs font-semibold hover:text-accent"
                  >
                    <span className="truncate font-mono">{f.name}</span>
                    <Icon
                      className={cn("size-3.5 shrink-0", !active && "text-muted")}
                      aria-hidden="true"
                    />
                  </button>
                  <span className="truncate font-mono text-[10px] text-muted">
                    {f.type._type === "Value" ? f.type.dtype : (f.type._type ?? "struct")}
                  </span>
                  <ColumnStatsButton stats={stats?.[f.name]} column={f.name} />
                </div>
              );
            })}
          </div>
          <div role="rowgroup" className="relative" style={{ height: v.getTotalSize() }}>
            {v.getVirtualItems().map((item) => {
              const r = data.rows[item.index];
              if (!r) return null;
              return (
                <div
                  key={r.row_idx}
                  role="row"
                  aria-rowindex={r.row_idx + 2}
                  data-index={item.index}
                  ref={v.measureElement}
                  className="absolute inset-x-0 top-0 grid border-b border-line text-[13px]"
                  style={{
                    gridTemplateColumns: template,
                    transform: `translateY(${item.start}px)`,
                  }}
                >
                  <div role="cell" className="tabular px-3 py-2 font-mono text-xs text-muted">
                    {r.row_idx}
                  </div>
                  {data.features.map((f) => (
                    <div key={f.name} role="cell" className="min-w-0 px-3 py-2">
                      <Cell
                        value={r.row[f.name]}
                        type={f.type}
                        truncated={r.truncated_cells.includes(f.name)}
                        label={`${f.name}, row ${r.row_idx}`}
                      />
                    </div>
                  ))}
                </div>
              );
            })}
          </div>
        </div>
      </div>
    </ScrollRegion>
  );
}

/** The dataset Viewer tab (DATA-6): pickers, search, filter, sort, statistics and paged rows. */
export function ViewerPage({
  ns,
  name,
  config,
  split,
}: {
  ns: string;
  name: string;
  config?: string;
  split?: string;
}) {
  const ref = { kind: "dataset" as const, ns, name };
  const repo = useRepo(ref);
  const configs = useDatasetConfigs(ref);
  const go = useGo();
  const [search, update] = useSearchState<ViewerSearch>();
  const [showFilter, setShowFilter] = useState(Boolean(search.where));
  const dataset = repo.data?.id ?? `${ns}/${name}`;
  const list = configs.data?.configs ?? [];
  const cfg = list.find((c) => c.config === config) ?? list.find((c) => c.default) ?? list[0];
  const sp = cfg?.splits.find((s) => s.split === split) ?? cfg?.splits[0];
  const page = Math.max(0, Number(search.page ?? 0) || 0);
  const sort = parseOrderBy(search.orderby);
  const ready = sp?.status === "ready" || sp?.status === "partial";
  const base = `${repoPath("dataset", ns, name)}/viewer`;
  const rows = useQuery({
    queryKey: [
      "viewer",
      dataset,
      cfg?.config,
      sp?.split,
      search.q,
      search.where,
      search.orderby,
      page,
    ],
    enabled: Boolean(cfg && sp && ready),
    placeholderData: keepPreviousData,
    queryFn: () =>
      fetchRows({
        dataset,
        config: cfg?.config ?? "",
        split: sp?.split ?? "",
        offset: page * pageSize,
        length: pageSize,
        query: search.q,
        where: search.where,
        orderby: search.orderby,
      }),
  });
  const stats = useQuery({
    queryKey: ["viewer-stats", dataset, cfg?.config, sp?.split],
    enabled: Boolean(cfg && sp && ready),
    queryFn: () => fetchStatistics(dataset, cfg?.config ?? "", sp?.split ?? ""),
    retry: false,
  });
  const size = useQuery({
    queryKey: ["viewer-size", dataset, cfg?.config],
    enabled: Boolean(cfg && ready),
    queryFn: () => fetchSize(dataset, cfg?.config ?? ""),
    retry: false,
  });
  if (configs.isPending) return <LoadingRows rows={5} label="Loading Configs" />;
  if (configs.isError && errorStatus(configs.error) === 422) {
    // Not a failure: the dataset holds files the Viewer does not read, such
    // as sample data for another project. Say so and point at the files.
    return (
      <EmptyState
        icon={<FileSearch />}
        title="Nothing to preview"
        action={
          repo.data ? (
            <Button asChild>
              <AppLink href={treePath("dataset", ns, name, repo.data.default_branch)}>
                Browse the files
              </AppLink>
            </Button>
          ) : null
        }
      >
        <p>
          The Viewer previews rows from data files that the datasets library reads, such as Parquet,
          CSV, JSON Lines, text and Arrow files, WebDataset archives, and folders of images or
          audio. This dataset has none it can read, which is normal for a dataset that holds other
          files. Download them from the Files tab, or with the snippets under Use this dataset.
        </p>
        <p className="mt-2 text-xs">{errorMessage(configs.error)}</p>
      </EmptyState>
    );
  }
  if (configs.isError)
    return <ErrorState error={configs.error} onRetry={() => configs.refetch()} />;
  if (!cfg || !sp) {
    return (
      <EmptyState title="No Configs or Splits yet">
        Hangar finds Splits from the card's <code className="font-mono">configs</code> metadata or
        from file names such as <code className="font-mono">train.csv</code>. Add data files, or
        edit the dataset card to declare Configs.
      </EmptyState>
    );
  }
  const statsByColumn = Object.fromEntries(
    (stats.data?.statistics ?? []).map((s) => [s.column_name, s]),
  );
  const sizeInfo = size.data?.size.splits.find((s) => s.split === sp.split);
  const total = rows.data?.num_rows_total ?? 0;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const go2 = (c: string, s: string) =>
    go(`${base}/${encodeURIComponent(c)}/${encodeURIComponent(s)}`);
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-3">
        <label className="space-y-1">
          <span className="block text-xs text-muted">Config</span>
          <Select
            className="w-auto font-mono text-xs"
            value={cfg.config}
            onChange={(e) => {
              const next = list.find((c) => c.config === e.target.value);
              go2(e.target.value, next?.splits[0]?.split ?? "");
            }}
          >
            {list.map((c) => (
              <option key={c.config} value={c.config}>
                {c.config}
              </option>
            ))}
          </Select>
        </label>
        <label className="space-y-1">
          <span className="block text-xs text-muted">Split</span>
          <Select
            className="w-auto font-mono text-xs"
            value={sp.split}
            onChange={(e) => go2(cfg.config, e.target.value)}
          >
            {cfg.splits.map((s) => (
              <option key={s.split} value={s.split}>
                {s.split}
                {s.num_rows !== undefined ? ` (${formatCount(s.num_rows)})` : ""}
              </option>
            ))}
          </Select>
        </label>
        <dl className="tabular flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
          <div className="flex gap-1">
            <dt>Rows</dt>
            <dd className="font-mono text-ink">
              {sp.num_rows !== undefined ? formatCount(sp.num_rows) : "Unknown"}
            </dd>
          </div>
          {sizeInfo || sp.num_bytes_parquet ? (
            <div className="flex gap-1">
              <dt>Parquet size</dt>
              <dd className="font-mono text-ink">
                {formatBytes(sizeInfo?.num_bytes_parquet_files ?? sp.num_bytes_parquet)}
              </dd>
            </div>
          ) : null}
          <div className="flex gap-1">
            <dt>Parquet export</dt>
            <dd>
              <Badge
                tone={
                  sp.status === "ready"
                    ? "ok"
                    : sp.status === "failed"
                      ? "danger"
                      : sp.status === "partial"
                        ? "warn"
                        : "info"
                }
              >
                {sp.status === "ready"
                  ? "Ready"
                  : sp.status === "partial"
                    ? "Partial"
                    : sp.status === "failed"
                      ? "Failed"
                      : "Preparing"}
              </Badge>
            </dd>
          </div>
        </dl>
      </div>
      {sp.status === "failed" ? (
        <ErrorState
          error={
            new Error(
              sp.error ||
                "The Parquet export failed. Ask an admin to check the dataset.convert Job.",
            )
          }
        />
      ) : !ready ? (
        <Preparing split={sp} />
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <form
              className="flex min-w-0 flex-1 basis-64 gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                const q = new FormData(e.currentTarget).get("q");
                update({
                  q: typeof q === "string" && q.trim() ? q.trim() : undefined,
                  where: undefined,
                  orderby: undefined,
                  page: undefined,
                });
              }}
            >
              <Input
                name="q"
                type="search"
                key={search.q ?? ""}
                defaultValue={search.q ?? ""}
                aria-label="Search the rows"
                placeholder="Search the rows"
              />
              <Button type="submit" aria-label="Search">
                <Search aria-hidden="true" />
              </Button>
            </form>
            <Button
              aria-expanded={showFilter}
              onClick={() => setShowFilter(!showFilter)}
              variant={search.where ? "primary" : "secondary"}
            >
              <Filter aria-hidden="true" />
              Filter
            </Button>
          </div>
          {showFilter && rows.data ? (
            <FilterBar
              features={rows.data.features}
              where={search.where ?? ""}
              onApply={(where) =>
                update({ where: where || undefined, q: undefined, page: undefined })
              }
            />
          ) : null}
          {search.q || search.where ? (
            <p className="text-xs text-muted">
              {search.q ? (
                <>
                  Rows where any text contains every word of{" "}
                  <span className="font-mono text-ink">{search.q}</span>
                </>
              ) : (
                <>
                  Rows where <code className="font-mono text-ink">{search.where}</code>
                </>
              )}
              {rows.data?.partial ? ". The scan stopped early, so there could be more." : ""}
            </p>
          ) : null}
          {rows.isPending ? (
            <LoadingRows rows={8} label="Loading rows" />
          ) : rows.isError ? (
            rows.error instanceof ViewerError && rows.error.notReady ? (
              <Preparing split={sp} />
            ) : (
              <ErrorState error={rows.error} onRetry={() => rows.refetch()} />
            )
          ) : rows.data.rows.length === 0 ? (
            <EmptyState title="No rows match">Change the search or the filter.</EmptyState>
          ) : (
            <>
              <RowsTable
                data={rows.data}
                stats={statsByColumn}
                sort={sort}
                onSort={(col) =>
                  update({
                    orderby: buildOrderBy(nextSort(sort, col)) || undefined,
                    q: undefined,
                    page: undefined,
                  })
                }
              />
              <nav
                aria-label="Pages of rows"
                className="flex flex-wrap items-center justify-between gap-2 text-sm"
              >
                <span className="tabular text-muted">
                  Rows {formatCount(page * pageSize + 1)} to{" "}
                  {formatCount(Math.min(total, (page + 1) * pageSize))} of {formatCount(total)}
                  {rows.data.partial ? " or more" : ""}
                </span>
                <span className="flex gap-2">
                  <Button
                    size="sm"
                    disabled={page === 0}
                    onClick={() => update({ page: page > 1 ? String(page - 1) : undefined })}
                  >
                    Previous page
                  </Button>
                  <Button
                    size="sm"
                    disabled={page + 1 >= pages}
                    onClick={() => update({ page: String(page + 1) })}
                  >
                    Next page
                  </Button>
                </span>
              </nav>
            </>
          )}
        </>
      )}
    </div>
  );
}
