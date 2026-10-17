import { useInfiniteQuery } from "@tanstack/react-query";
import { Download, Heart, Telescope } from "lucide-react";
import { api } from "@/api/client";
import type { DiscoverItem } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Input, Select } from "@/components/ui/form";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { formatCompact, relativeTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { datasetFilters, modelFilters, toFilterParams } from "@/lib/pull";
import { useSearchState } from "@/lib/use-search-state";
import { cn } from "@/lib/utils";
import { ArsenalBadge } from "../repos/badges";
import { useUpstreams } from "../repos/useInstance";
import { DiscoverDetailPanel } from "./DiscoverDetail";

type DiscoverSearch = {
  type?: string;
  upstream?: string;
  q?: string;
  author?: string;
  sort?: string;
  repo?: string;
  format?: string;
  task?: string;
  library?: string;
  modality?: string;
  task_categories?: string;
  size_categories?: string;
};

const sorts = [
  { value: "trending", label: "Trending" },
  { value: "downloads", label: "Most downloads" },
  { value: "likes", label: "Most likes" },
  { value: "lastModified", label: "Recently updated" },
] as const;

const facetLabels: Record<string, string> = {
  format: "Format",
  task: "Task",
  library: "Library",
  modality: "Modality",
  task_categories: "Task",
  size_categories: "Rows",
};

function Result({ item, onOpen }: { item: DiscoverItem; onOpen: () => void }) {
  const updateAvailable = Boolean(item.local_sha && item.sha && item.local_sha !== item.sha);
  return (
    <li className="relative flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-3 hover:bg-raised/60">
      <div className="min-w-0 flex-1">
        <button
          type="button"
          onClick={onOpen}
          className="text-left font-mono text-sm font-medium after:absolute after:inset-0 hover:underline"
        >
          {item.id}
        </button>
        <div className="mt-1 flex flex-wrap items-center gap-1.5 text-xs text-muted">
          <ArsenalBadge state={item.arsenal} updateAvailable={updateAvailable} />
          {item.pipeline_tag ? <Badge>{item.pipeline_tag}</Badge> : null}
          {item.gated ? <Badge tone="warn">Gated</Badge> : null}
          {item.last_modified ? <span>Updated {relativeTime(item.last_modified)}</span> : null}
        </div>
      </div>
      <dl className="tabular flex gap-3 text-xs text-muted">
        <div className="flex items-center gap-1">
          <dt>
            <Download className="size-3" aria-label="Downloads" />
          </dt>
          <dd>{formatCompact(item.downloads)}</dd>
        </div>
        <div className="flex items-center gap-1">
          <dt>
            <Heart className="size-3" aria-label="Likes" />
          </dt>
          <dd>{formatCompact(item.likes)}</dd>
        </div>
      </dl>
    </li>
  );
}

/**
 * repoIdQuery returns the query as a Repo ID when it has the form
 * `namespace/name`, so Discover can open that Repo directly. Upstream
 * searches do not list every Repo: huggingface.co leaves some
 * organisations out of search, such as hf-internal-testing.
 */
export function repoIdQuery(q: string | undefined): string | undefined {
  const id = q?.trim();
  return id && /^[A-Za-z0-9][\w.-]*\/[A-Za-z0-9][\w.-]*$/.test(id) ? id : undefined;
}

/** Search Upstreams for models and datasets, marked with their Arsenal state (BROWSE-2, DATA-7). */
export function DiscoverPage() {
  const [search, update] = useSearchState<DiscoverSearch>();
  const type: "models" | "datasets" = search.type === "datasets" ? "datasets" : "models";
  const upstreams = useUpstreams();
  const upstream = search.upstream ?? upstreams.data?.items[0]?.name;
  const facets = type === "datasets" ? datasetFilters : modelFilters;
  const selected = Object.fromEntries(
    Object.keys(facets).map((k) => [k, search[k as keyof DiscoverSearch]]),
  );
  const filter = toFilterParams(type, selected);
  const list = useInfiniteQuery({
    queryKey: ["discover", type, upstream, search.q, search.author, search.sort, filter.join(",")],
    enabled: Boolean(upstream),
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/discover/{type}", {
          params: {
            path: { type },
            query: {
              upstream,
              ...(search.q ? { search: search.q } : {}),
              ...(search.author ? { author: search.author } : {}),
              ...(search.sort ? { sort: search.sort as (typeof sorts)[number]["value"] } : {}),
              ...(filter.length ? { filter } : {}),
              ...(pageParam ? { cursor: pageParam } : {}),
            },
          },
        }),
      ),
    getNextPageParam: (l) => l.next_cursor || undefined,
  });
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  const queryId = repoIdQuery(search.q);
  const exactId =
    queryId && !items.some((i) => i.id.toLowerCase() === queryId.toLowerCase())
      ? queryId
      : undefined;
  const tab = (t: "models" | "datasets", label: string) => (
    <button
      type="button"
      role="tab"
      aria-selected={type === t}
      onClick={() =>
        update({
          type: t === "models" ? undefined : t,
          repo: undefined,
          format: undefined,
          task: undefined,
          library: undefined,
          modality: undefined,
          task_categories: undefined,
          size_categories: undefined,
        })
      }
      className={cn(
        "-mb-px border-b-2 border-transparent px-3 py-2 text-sm text-muted hover:text-ink",
        type === t && "border-accent-fill font-medium text-ink",
      )}
    >
      {label}
    </button>
  );
  return (
    <PageShell
      title="Discover"
      subtitle="Search an Upstream and pull what you need into your arsenal."
    >
      <div role="tablist" aria-label="Repo type" className="flex gap-1 border-b border-line">
        {tab("models", "Models")}
        {tab("datasets", "Datasets")}
      </div>
      <div className="flex flex-wrap items-end gap-2">
        <label className="space-y-1">
          <span className="block text-xs text-muted">Upstream</span>
          <Select
            className="w-auto"
            value={upstream ?? ""}
            onChange={(e) => update({ upstream: e.target.value })}
            disabled={!upstreams.data}
          >
            {(upstreams.data?.items ?? []).map((u) => (
              <option key={u.name} value={u.name}>
                {u.display_name}
              </option>
            ))}
          </Select>
        </label>
        <form
          className="min-w-0 flex-1 basis-60 space-y-1"
          onSubmit={(e) => {
            e.preventDefault();
            const q = new FormData(e.currentTarget).get("q");
            update({ q: typeof q === "string" ? q.trim() : undefined, repo: undefined });
          }}
        >
          <label htmlFor="discover-q" className="block text-xs text-muted">
            Search
          </label>
          <Input
            id="discover-q"
            name="q"
            type="search"
            key={search.q ?? ""}
            defaultValue={search.q ?? ""}
            placeholder={
              type === "datasets" ? "imdb, squad, speech" : "llama gguf, qwen, embeddings"
            }
          />
        </form>
        <label className="space-y-1">
          <span className="block text-xs text-muted">Sort</span>
          <Select
            className="w-auto"
            value={search.sort ?? "trending"}
            onChange={(e) =>
              update({ sort: e.target.value === "trending" ? undefined : e.target.value })
            }
          >
            {sorts.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </Select>
        </label>
      </div>
      <div className="flex flex-wrap gap-2">
        {Object.entries(facets).map(([facet, values]) => (
          <label key={facet} className="flex items-center gap-1.5 text-sm">
            <span className="text-muted">{facetLabels[facet]}</span>
            <Select
              className="h-8 w-auto text-xs"
              value={search[facet as keyof DiscoverSearch] ?? ""}
              onChange={(e) => update({ [facet]: e.target.value || undefined })}
            >
              <option value="">Any</option>
              {values.map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </Select>
          </label>
        ))}
      </div>
      {exactId && upstreams.data?.items.length ? (
        <p className="text-sm">
          <Button variant="secondary" size="sm" onClick={() => update({ repo: exactId })}>
            Open <span className="font-mono">{exactId}</span>
          </Button>
        </p>
      ) : null}
      {upstreams.isSuccess && !upstreams.data.items.length ? (
        <EmptyState icon={<Telescope />} title="No Upstreams are enabled">
          Ask an admin to enable huggingface.co or add another Upstream.
        </EmptyState>
      ) : list.isPending ? (
        <LoadingRows rows={6} label="Searching the Upstream" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState title="Nothing found">Try other words or fewer filters.</EmptyState>
      ) : (
        <>
          <ul
            aria-label="Results"
            className="divide-y divide-line rounded-lg border border-line bg-surface"
          >
            {items.map((i) => (
              <Result key={i.id} item={i} onOpen={() => update({ repo: i.id })} />
            ))}
          </ul>
          {list.hasNextPage ? (
            <Button onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
              More results
            </Button>
          ) : null}
        </>
      )}
      <Dialog
        open={Boolean(search.repo)}
        onOpenChange={(o) => {
          if (!o) update({ repo: undefined });
        }}
        side="right"
        wide
        title={<span className="font-mono">{search.repo}</span>}
        description={`On ${upstreams.data?.items.find((u) => u.name === upstream)?.display_name ?? upstream}`}
      >
        {search.repo && upstream ? (
          <DiscoverDetailPanel type={type} id={search.repo} upstream={upstream} />
        ) : null}
      </Dialog>
    </PageShell>
  );
}
