import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import {
  Boxes,
  Database,
  LayoutGrid,
  List,
  SlidersHorizontal,
  Telescope,
  Upload,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { api } from "@/api/client";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Input, Select } from "@/components/ui/form";
import { SegmentedControl } from "@/components/ui/segmented";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { VirtualGrid } from "@/components/VirtualGrid";
import { formatCount } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import type { RepoKind } from "@/lib/repo";
import { useSearchState } from "@/lib/use-search-state";
import { cn } from "@/lib/utils";
import { RepoCard } from "../repos/RepoCard";
import {
  activeFilterCount,
  contextOptions,
  facetGroupsFor,
  type ListSearch,
  paramBuckets,
  sizeOptions,
  sortOptionsFor,
  toggleListValue,
  toListQuery,
} from "./filters";

type Facets = Record<string, Record<string, number>>;

function FacetGroup({
  label,
  values,
  selected,
  onToggle,
}: {
  label: string;
  values: [string, number][];
  selected: string[];
  onToggle: (v: string) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  if (values.length === 0) return null;
  const shown = expanded ? values : values.slice(0, 6);
  return (
    <fieldset className="space-y-1">
      <legend className="mb-1 text-xs font-semibold text-muted">{label}</legend>
      {shown.map(([value, count]) => {
        const on = selected.includes(value);
        return (
          <button
            key={value}
            type="button"
            aria-pressed={on}
            onClick={() => onToggle(value)}
            className={cn(
              "flex w-full items-center justify-between gap-2 rounded-sm px-2 py-1 text-left text-sm transition-[color,background-color] duration-150 hover:bg-raised",
              on && "bg-accent-soft font-medium text-ink hover:bg-accent-soft",
            )}
          >
            <span className="truncate">{value}</span>
            <span className="tabular font-mono text-xs text-muted">{formatCount(count)}</span>
          </button>
        );
      })}
      {values.length > 6 ? (
        <Button variant="link" size="sm" onClick={() => setExpanded(!expanded)}>
          {expanded ? "Show fewer" : `Show all ${values.length}`}
        </Button>
      ) : null}
    </fieldset>
  );
}

function Filters({
  kind,
  search,
  update,
  facets,
}: {
  kind: RepoKind;
  search: ListSearch;
  update: (p: Partial<ListSearch>) => void;
  facets: Facets;
}) {
  const sorted = (name: string): [string, number][] =>
    Object.entries(facets[name] ?? {}).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  return (
    <div className="space-y-5">
      <div className="space-y-1">
        {/* The group's legend names it for assistive technology. */}
        <p aria-hidden="true" className="mb-1 text-xs font-semibold text-muted">
          Mirror or local
        </p>
        <SegmentedControl<string | undefined>
          legend="Mirror or local"
          className="w-fit"
          value={search.mirror}
          onValueChange={(v) => update({ mirror: v })}
          options={[
            { value: undefined, label: "Any" },
            { value: "true", label: "Mirrors" },
            { value: "false", label: "Local" },
          ]}
        />
      </div>
      {facetGroupsFor(kind).map((g) => {
        const current = search[g.param];
        const selected =
          g.param === "format"
            ? (current ?? "").split(",").filter(Boolean)
            : current
              ? [current]
              : [];
        return (
          <FacetGroup
            key={g.facet}
            label={g.label}
            values={sorted(g.facet)}
            selected={selected}
            onToggle={(v) =>
              update({
                [g.param]:
                  g.param === "format"
                    ? toggleListValue(current, v)
                    : current === v
                      ? undefined
                      : v,
              })
            }
          />
        );
      })}
      {kind === "model" ? (
        <>
          <label className="block space-y-1">
            <span className="text-xs font-semibold text-muted">Parameters</span>
            <Select
              value={search.params ?? ""}
              onChange={(e) => update({ params: e.target.value || undefined })}
            >
              <option value="">Any size</option>
              {paramBuckets.map((b) => (
                <option key={b.id} value={b.id}>
                  {b.label}
                </option>
              ))}
            </Select>
          </label>
          <label className="block space-y-1">
            <span className="text-xs font-semibold text-muted">Context length</span>
            <Select
              value={search.context_min ?? ""}
              onChange={(e) => update({ context_min: e.target.value || undefined })}
            >
              <option value="">Any length</option>
              {contextOptions.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </Select>
          </label>
        </>
      ) : null}
      <label className="block space-y-1">
        <span className="text-xs font-semibold text-muted">Download size</span>
        <Select
          value={search.size_max ?? ""}
          onChange={(e) => update({ size_max: e.target.value || undefined })}
        >
          <option value="">Any size</option>
          {sizeOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </label>
    </div>
  );
}

/** What changes between the Models and Datasets lists. */
const copy = {
  model: {
    title: "Models",
    subtitle: "Every model you can see in this Instance.",
    newLabel: "New model",
    newTo: "/new",
    discover: { type: undefined },
    filterLabel: "Filter models by name",
    loading: "Loading models",
    listLabel: "Models",
    emptyTitle: "Your arsenal has no models yet.",
    emptyText: "Discover a model to pull, or upload your own.",
    discoverAction: "Discover a model",
    noMatch: "Remove a filter or change the search to see more of your models.",
  },
  dataset: {
    title: "Datasets",
    subtitle:
      "Every dataset you can see in this Instance. A dataset holds any files: training and evaluation data, or sample data for other projects.",
    newLabel: "New dataset",
    newTo: "/new-dataset",
    discover: { type: "datasets" },
    filterLabel: "Filter datasets by name",
    loading: "Loading datasets",
    listLabel: "Datasets",
    emptyTitle: "Your arsenal has no datasets yet.",
    emptyText: "Discover a dataset to pull, or upload your own files.",
    discoverAction: "Discover a dataset",
    noMatch: "Remove a filter or change the search to see more of your datasets.",
  },
} as const;

/**
 * The Models or the Datasets list: the Repos of one kind the user can see,
 * with filters, sort, search and grid or list (BROWSE-1, DATA-11). The two
 * kinds never share a list.
 */
export function RepoListPage({ kind }: { kind: RepoKind }) {
  const [search, update] = useSearchState<ListSearch>();
  const [sheet, setSheet] = useState(false);
  const text = copy[kind];
  const query = toListQuery(search, kind);
  const list = useInfiniteQuery({
    queryKey: ["repos", query],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/repos", { params: { query: { ...query, cursor: pageParam, limit: 60 } } }),
      ),
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  const facets: Facets = list.data?.pages[0]?.facets ?? {};
  const view = search.view === "list" ? "list" : "grid";
  const filterCount = activeFilterCount(search, kind);
  const filtered = filterCount > 0 || Boolean(search.q);
  const filters = <Filters kind={kind} search={search} update={update} facets={facets} />;
  const sort = sortOptionsFor(kind).some((o) => o.value === search.sort)
    ? (search.sort ?? "updated")
    : "updated";
  // Fade the grid in on its first appearance only: a filter change remounts
  // it after the loading rows, and fading every result set would feel slow.
  const gridShown = useRef(false);
  const showsGrid = items.length > 0;
  useEffect(() => {
    if (showsGrid) gridShown.current = true;
  }, [showsGrid]);

  return (
    <PageShell
      title={text.title}
      subtitle={text.subtitle}
      actions={
        <>
          <Button asChild>
            <Link to={text.newTo}>
              <Upload aria-hidden="true" />
              {text.newLabel}
            </Link>
          </Button>
          <Button asChild variant="primary">
            <Link to="/discover" search={text.discover}>
              <Telescope aria-hidden="true" />
              Discover
            </Link>
          </Button>
        </>
      }
    >
      <div className="grid gap-6 lg:grid-cols-[240px_minmax(0,1fr)]">
        <aside aria-label="Filters" className="hidden lg:block">
          {filters}
        </aside>
        <div className="min-w-0 space-y-4">
          <div className="flex flex-wrap items-center gap-2">
            <form
              className="min-w-0 flex-1 basis-56"
              onSubmit={(e) => {
                e.preventDefault();
                const q = new FormData(e.currentTarget).get("q");
                update({ q: typeof q === "string" ? q : undefined });
              }}
            >
              <Input
                name="q"
                defaultValue={search.q ?? ""}
                key={search.q ?? ""}
                type="search"
                placeholder="Filter by name"
                aria-label={text.filterLabel}
              />
            </form>
            <Dialog
              open={sheet}
              onOpenChange={setSheet}
              side="right"
              title="Filters"
              trigger={
                <Button className="lg:hidden">
                  <SlidersHorizontal aria-hidden="true" />
                  Filters{filterCount ? ` (${filterCount})` : ""}
                </Button>
              }
            >
              {filters}
            </Dialog>
            <label className="flex items-center gap-2 text-sm">
              <span className="text-muted">Sort</span>
              <Select
                value={sort}
                onChange={(e) =>
                  update({ sort: e.target.value === "updated" ? undefined : e.target.value })
                }
                className="w-auto"
              >
                {sortOptionsFor(kind).map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </Select>
            </label>
            <fieldset className="flex rounded-md border border-line-strong">
              <legend className="sr-only">Layout</legend>
              <Button
                variant="ghost"
                size="icon"
                aria-label="Grid"
                aria-pressed={view === "grid"}
                className={cn("rounded-r-none", view === "grid" && "bg-raised")}
                onClick={() => update({ view: undefined })}
              >
                <LayoutGrid aria-hidden="true" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                aria-label="List"
                aria-pressed={view === "list"}
                className={cn("rounded-l-none", view === "list" && "bg-raised")}
                onClick={() => update({ view: "list" })}
              >
                <List aria-hidden="true" />
              </Button>
            </fieldset>
          </div>
          {list.isPending ? (
            <LoadingRows rows={6} label={text.loading} />
          ) : list.isError ? (
            <ErrorState error={list.error} onRetry={() => list.refetch()} />
          ) : items.length === 0 ? (
            filtered ? (
              <EmptyState title="Nothing matches these filters">{text.noMatch}</EmptyState>
            ) : (
              <EmptyState
                icon={kind === "dataset" ? <Database /> : <Boxes />}
                title={text.emptyTitle}
                action={
                  <>
                    <Button asChild variant="primary">
                      <Link to="/discover" search={text.discover}>
                        {text.discoverAction}
                      </Link>
                    </Button>
                    <Button asChild>
                      <Link to={text.newTo}>Upload your own</Link>
                    </Button>
                  </>
                }
              >
                {text.emptyText}
              </EmptyState>
            )
          ) : (
            <>
              {/* Each card has an h3 title, so the grid needs an h2 above it. */}
              <h2 className="sr-only">{text.listLabel}</h2>
              <VirtualGrid
                label={text.listLabel}
                fadeIn={!gridShown.current}
                items={items}
                itemKey={(r) => `${r.type}/${r.id}`}
                minColumnWidth={view === "list" ? 10_000 : 300}
                estimateRowHeight={view === "list" ? 76 : 140}
                render={(r) => <RepoCard repo={r} layout={view} />}
                onEnd={() => list.hasNextPage && !list.isFetchingNextPage && list.fetchNextPage()}
              />
              {list.hasNextPage ? (
                <div className="flex justify-center">
                  <Button onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                    {list.isFetchingNextPage ? "Loading" : "Load more"}
                  </Button>
                </div>
              ) : null}
            </>
          )}
        </div>
      </div>
    </PageShell>
  );
}
