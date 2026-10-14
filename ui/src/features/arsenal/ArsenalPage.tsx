import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Boxes, LayoutGrid, List, SlidersHorizontal, Telescope, Upload } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Input, Select } from "@/components/ui/form";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { VirtualGrid } from "@/components/VirtualGrid";
import { formatCount } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { useSearchState } from "@/lib/use-search-state";
import { cn } from "@/lib/utils";
import { RepoCard } from "../repos/RepoCard";
import {
  type ArsenalSearch,
  activeFilterCount,
  contextOptions,
  facetGroups,
  paramBuckets,
  sizeOptions,
  sortOptions,
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
              "flex w-full items-center justify-between gap-2 rounded-sm px-2 py-1 text-left text-sm hover:bg-raised",
              on && "bg-accent-soft font-medium text-ink",
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
  search,
  update,
  facets,
}: {
  search: ArsenalSearch;
  update: (p: Partial<ArsenalSearch>) => void;
  facets: Facets;
}) {
  const sorted = (name: string): [string, number][] =>
    Object.entries(facets[name] ?? {}).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const type = search.type;
  return (
    <div className="space-y-5">
      <fieldset className="space-y-1">
        <legend className="mb-1 text-xs font-semibold text-muted">Type</legend>
        <div className="flex gap-1">
          {[
            { v: undefined, l: "All" },
            { v: "models", l: "Models" },
            { v: "datasets", l: "Datasets" },
          ].map((o) => (
            <Button
              key={o.l}
              size="sm"
              variant={type === o.v ? "primary" : "secondary"}
              aria-pressed={type === o.v}
              onClick={() => update({ type: o.v })}
            >
              {o.l}
            </Button>
          ))}
        </div>
      </fieldset>
      <fieldset className="space-y-1">
        <legend className="mb-1 text-xs font-semibold text-muted">Mirror or local</legend>
        <div className="flex flex-wrap gap-1">
          {[
            { v: undefined, l: "Any" },
            { v: "true", l: "Mirrors" },
            { v: "false", l: "Local" },
          ].map((o) => (
            <Button
              key={o.l}
              size="sm"
              variant={search.mirror === o.v ? "primary" : "secondary"}
              aria-pressed={search.mirror === o.v}
              onClick={() => update({ mirror: o.v })}
            >
              {o.l}
            </Button>
          ))}
        </div>
      </fieldset>
      {facetGroups
        .filter(
          (g) => !(g.datasetsOnly && type === "models") && !(g.modelsOnly && type === "datasets"),
        )
        .map((g) => {
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
      {type !== "datasets" ? (
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

/** Repos the user can see, with filters, sort, search and grid or list (BROWSE-1, DATA-11). */
export function ArsenalPage() {
  const [search, update] = useSearchState<ArsenalSearch>();
  const [sheet, setSheet] = useState(false);
  const query = toListQuery(search);
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
  const filterCount = activeFilterCount(search);
  const filtered = filterCount > 0 || Boolean(search.q);
  const filters = <Filters search={search} update={update} facets={facets} />;

  return (
    <PageShell
      title="Arsenal"
      subtitle="Every model and dataset you can see in this Instance."
      actions={
        <>
          <Button asChild>
            <Link to="/new">
              <Upload aria-hidden="true" />
              New model
            </Link>
          </Button>
          <Button asChild variant="primary">
            <Link to="/discover">
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
                aria-label="Filter the arsenal by name"
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
                value={search.sort ?? "updated"}
                onChange={(e) =>
                  update({ sort: e.target.value === "updated" ? undefined : e.target.value })
                }
                className="w-auto"
              >
                {sortOptions.map((o) => (
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
            <LoadingRows rows={6} label="Loading the arsenal" />
          ) : list.isError ? (
            <ErrorState error={list.error} onRetry={() => list.refetch()} />
          ) : items.length === 0 ? (
            filtered ? (
              <EmptyState title="Nothing matches these filters">
                Remove a filter or change the search to see more of your arsenal.
              </EmptyState>
            ) : (
              <EmptyState
                icon={<Boxes />}
                title="Your arsenal is empty."
                action={
                  <>
                    <Button asChild variant="primary">
                      <Link to="/discover">Discover a model</Link>
                    </Button>
                    <Button asChild>
                      <Link to="/new">Upload your own</Link>
                    </Button>
                  </>
                }
              >
                Discover a model to pull, or upload your own.
              </EmptyState>
            )
          ) : (
            <>
              {/* Each card has an h3 title, so the grid needs an h2 above it. */}
              <h2 className="sr-only">Repos</h2>
              <VirtualGrid
                label="Repos"
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
