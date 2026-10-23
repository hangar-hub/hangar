import { useMutation, useQuery } from "@tanstack/react-query";
import { FilePlus, Pencil } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Dependency, Repo } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { LazyMarkdown } from "@/components/content/LazyMarkdown";
import { Button } from "@/components/ui/button";
import { InfoBadge } from "@/components/ui/info-badge";
import { EmptyState, ErrorState, Skeleton } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatCompact, formatContext, formatCount, formatParams } from "@/lib/format";
import { parseCardData, splitFrontMatter } from "@/lib/frontmatter";
import { errorMessage, errorStatus, unwrap } from "@/lib/problem";
import { blobPath, parseRepoId, type RepoKind, repoPath, resolveUrl, treePath } from "@/lib/repo";
import { CardEditor } from "../datasets/CardEditor";
import {
  ArsenalBadge,
  DependencyKindBadge,
  DependencyPullBadge,
  formatName,
  QuantChips,
} from "./badges";
import {
  type RepoRef,
  repoKey,
  repoPathParams,
  useDatasetConfigs,
  useDependencies,
  useMetadata,
  useRepo,
  useTextFile,
} from "./hooks";
import { sizeText } from "./RepoCard";
import { useExternalImages, useUpstreams } from "./useInstance";

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 py-1.5">
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="min-w-0 text-right text-sm">{children}</dd>
    </div>
  );
}

function SidebarSection({ title, children }: { title: string; children: React.ReactNode }) {
  // An ID without spaces: aria-labelledby reads spaces as a list of IDs.
  const id = `side-${title.toLowerCase().replace(/\W+/g, "-")}`;
  return (
    <section aria-labelledby={id} className="border-t border-line pt-3">
      <h2 id={id} className="mb-1 text-sm font-semibold">
        {title}
      </h2>
      {children}
    </section>
  );
}

const kindLabels: Record<Dependency["kind"], string> = {
  code: "Code",
  base_model: "Base model",
  adapter_base: "Adapter base",
  card_base_model: "Base model",
  dataset: "Dataset",
};

function DependencyItem({ dep, mirrorUpstream }: { dep: Dependency; mirrorUpstream?: string }) {
  const toast = useToast();
  const upstreams = useUpstreams(dep.arsenal !== "full");
  const pull = useMutation({
    mutationFn: async () => {
      const upstream = mirrorUpstream ?? upstreams.data?.items[0]?.name;
      if (!upstream) throw new Error("No Upstream is enabled. Ask an admin to enable one.");
      return unwrap(
        await api.POST("/pulls", {
          body: { upstream, repo: dep.repo, type: dep.type, revision: dep.revision },
        }),
      );
    },
    onSuccess: () => toast(`Pull of ${dep.repo} started. Follow it in Jobs.`),
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const id = parseRepoId(dep.repo, dep.type);
  return (
    <li className="space-y-1 py-1.5">
      <div className="flex flex-wrap items-center gap-1.5">
        {id && dep.arsenal !== "none" ? (
          <AppLink
            href={repoPath(id.kind, id.ns, id.name)}
            className="break-all font-mono text-sm hover:underline"
          >
            {dep.repo}
          </AppLink>
        ) : (
          <span className="break-all font-mono text-sm">{dep.repo}</span>
        )}
        <DependencyKindBadge kind={dep.kind} label={kindLabels[dep.kind]} />
        {dep.required ? <DependencyPullBadge required label="Required" /> : null}
        <ArsenalBadge state={dep.arsenal} />
      </div>
      {dep.arsenal !== "full" && dep.kind !== "dataset" ? (
        <Button size="sm" onClick={() => pull.mutate()} disabled={pull.isPending || pull.isSuccess}>
          {pull.isSuccess ? "Pull started" : "Pull into arsenal"}
        </Button>
      ) : null}
    </li>
  );
}

function Sidebar({ repo, cardData }: { repo: Repo; cardData: Record<string, unknown> }) {
  const ref: RepoRef = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const meta = useMetadata(ref);
  const deps = useDependencies(ref);
  const configs = useDatasetConfigs(ref);
  const collections = useQuery({
    queryKey: ["collections", "item", repo.type, repo.id],
    queryFn: async () =>
      unwrap(
        await api.GET("/collections", { params: { query: { item: `${repo.type}s/${repo.id}` } } }),
      ),
  });
  const s = repo.summary ?? {};
  const summary = (meta.data?.summary ?? {}) as Record<string, unknown>;
  const trainedOn = deps.data?.needs.filter((d) => d.kind === "dataset") ?? [];
  const needs = deps.data?.needs.filter((d) => d.kind !== "dataset") ?? [];
  const neededBy = deps.data?.needed_by ?? [];
  const isDataset = repo.type === "dataset";
  return (
    <aside aria-label="About this Repo" className="space-y-4">
      {repo.description ? <p className="text-sm">{repo.description}</p> : null}
      <section aria-labelledby="side-details">
        <h2 id="side-details" className="sr-only">
          Details
        </h2>
        {meta.data?.status === "pending" ? (
          <p className="mb-2">
            <InfoBadge
              tone="info"
              explanation={{
                title: "Indexing",
                body: (
                  <p>
                    Hangar is reading this Commit's files for their format, architecture, parameter
                    count, quantisation and licence. The details below fill in when it finishes.
                  </p>
                ),
              }}
            >
              Indexing
            </InfoBadge>
          </p>
        ) : null}
        <dl className="divide-y divide-line">
          {s.formats?.length ? (
            <Row label="Format">{s.formats.map(formatName).join(", ")}</Row>
          ) : null}
          {s.architecture ? (
            <Row label="Architecture">
              <span className="font-mono text-xs">{s.architecture}</span>
            </Row>
          ) : null}
          {s.parameters ? (
            <Row label="Parameters">
              <span className="font-mono">{formatParams(s.parameters)}</span>
            </Row>
          ) : null}
          {s.quants?.length ? (
            <Row label="Quants">
              <span className="flex flex-wrap justify-end gap-1">
                <QuantChips quants={s.quants} max={8} />
              </span>
            </Row>
          ) : null}
          {s.context_length ? (
            <Row label="Context length">
              <span className="font-mono">{formatContext(s.context_length)}</span>
            </Row>
          ) : null}
          {repo.licence ? <Row label="Licence">{repo.licence}</Row> : null}
          {s.pipeline_tag ? <Row label="Task">{s.pipeline_tag}</Row> : null}
          {s.library_name ? <Row label="Library">{s.library_name}</Row> : null}
          {typeof summary.size_category === "string" ? (
            <Row label="Rows">{summary.size_category}</Row>
          ) : null}
          <Row label="Size">
            <span className="font-mono">{sizeText(repo)}</span>
          </Row>
          <Row label="Downloads, last 30 days">
            <span className="tabular font-mono">{formatCompact(repo.downloads)}</span>
          </Row>
        </dl>
      </section>
      {isDataset ? (
        <SidebarSection title="Configs and Splits">
          {configs.isPending ? (
            <Skeleton className="h-16" />
          ) : configs.data?.configs.length ? (
            <ul className="space-y-2">
              {configs.data.configs.map((c) => (
                <li key={c.config}>
                  <p className="font-mono text-sm">
                    {c.config}
                    {c.default ? (
                      <span className="ml-1.5 text-xs text-muted">(default)</span>
                    ) : null}
                  </p>
                  <ul className="mt-1 space-y-0.5">
                    {c.splits.map((sp) => (
                      <li
                        key={sp.split}
                        className="flex items-baseline justify-between gap-2 text-xs"
                      >
                        <AppLink
                          href={`${repoPath("dataset", repo.namespace, repo.name)}/viewer/${encodeURIComponent(c.config)}/${encodeURIComponent(sp.split)}`}
                          className="inline-block min-h-6 py-0.5 font-mono hover:underline"
                        >
                          {sp.split}
                        </AppLink>
                        <span className="tabular text-muted">
                          {sp.num_rows !== undefined
                            ? `${formatCount(sp.num_rows)} rows`
                            : sp.status === "pending"
                              ? "Preparing"
                              : ""}
                          {sp.num_bytes_parquet ? `, ${formatBytes(sp.num_bytes_parquet)}` : ""}
                        </span>
                      </li>
                    ))}
                  </ul>
                </li>
              ))}
            </ul>
          ) : configs.isError && errorStatus(configs.error) === 422 ? (
            <p className="text-sm text-muted">
              None. The Viewer has no data files it can read here, so the files are in the{" "}
              <AppLink
                href={treePath("dataset", repo.namespace, repo.name, repo.default_branch)}
                className="hg-link"
              >
                Files
              </AppLink>{" "}
              tab.
            </p>
          ) : (
            <p className="text-sm text-muted">No Configs found yet.</p>
          )}
        </SidebarSection>
      ) : null}
      {isDataset && Object.keys(cardData).length ? (
        <SidebarSection title="Card metadata">
          <dl className="divide-y divide-line">
            {Object.entries(cardData)
              .filter(([k]) => k !== "configs" && k !== "dataset_info")
              .map(([k, v]) => (
                <Row key={k} label={k}>
                  <span className="break-words font-mono text-xs">
                    {Array.isArray(v)
                      ? v.join(", ")
                      : typeof v === "object"
                        ? JSON.stringify(v)
                        : String(v)}
                  </span>
                </Row>
              ))}
          </dl>
        </SidebarSection>
      ) : null}
      {needs.length || (!isDataset && neededBy.length) ? (
        <SidebarSection title="Dependencies">
          {needs.length ? (
            <>
              <h3 className="text-xs text-muted">Needs</h3>
              <ul>
                {needs.map((d) => (
                  <DependencyItem
                    key={`${d.kind}:${d.repo}`}
                    dep={d}
                    mirrorUpstream={repo.mirror_status?.upstream}
                  />
                ))}
              </ul>
            </>
          ) : null}
          {!isDataset && neededBy.length ? (
            <>
              <h3 className="mt-2 text-xs text-muted">Needed by</h3>
              <ul>
                {neededBy.map((d) => (
                  <DependencyItem key={`by:${d.repo}`} dep={d} />
                ))}
              </ul>
            </>
          ) : null}
        </SidebarSection>
      ) : null}
      {trainedOn.length ? (
        <SidebarSection title="Datasets used">
          <ul>
            {trainedOn.map((d) => (
              <DependencyItem key={d.repo} dep={d} />
            ))}
          </ul>
        </SidebarSection>
      ) : !isDataset && deps.isSuccess && repo.permissions?.admin && !repo.mirror ? (
        <SidebarSection title="Datasets used">
          <p className="text-sm text-muted">
            No datasets listed.{" "}
            <AppLink
              href={`${repoPath(repo.type, repo.namespace, repo.name)}/settings`}
              className="hg-link"
            >
              Choose datasets
            </AppLink>
          </p>
        </SidebarSection>
      ) : null}
      {isDataset && neededBy.length ? (
        <SidebarSection title="Used by">
          <ul>
            {neededBy.map((d) => (
              <DependencyItem key={d.repo} dep={d} />
            ))}
          </ul>
        </SidebarSection>
      ) : null}
      {collections.data?.items.length ? (
        <SidebarSection title="In Collections">
          <ul className="space-y-1">
            {collections.data.items.map((c) => (
              <li key={`${c.namespace}/${c.slug}`}>
                <AppLink
                  href={`/collections/${c.namespace}/${c.slug}`}
                  className="text-sm hover:underline"
                >
                  {c.title}
                </AppLink>
                <span className="ml-1.5 font-mono text-xs text-muted">{c.namespace}</span>
              </li>
            ))}
          </ul>
        </SidebarSection>
      ) : null}
    </aside>
  );
}

/** The Model card or Dataset card tab: the rendered README and the sidebar (BROWSE-3, DATA-13). */
export function CardPage({ kind, ns, name }: { kind: RepoKind; ns: string; name: string }) {
  const ref: RepoRef = { kind, ns, name };
  const repo = useRepo(ref);
  const r = repo.data;
  const rev = r?.default_branch ?? "main";
  // Read the card at the head Commit the Repo reports, so the text and the
  // parent Commit the card editor sends always match.
  const readme = useTextFile(ref, r?.sha || rev, "README.md", Boolean(r));
  const externalImages = useExternalImages();
  const [editing, setEditing] = useState(false);
  if (!r) return null;
  const split = splitFrontMatter(readme.data ?? "");
  let cardData: Record<string, unknown> = {};
  let yamlError = "";
  try {
    cardData = parseCardData(split.yaml);
  } catch (e) {
    yamlError = errorMessage(e);
  }
  const canWrite = Boolean(r.permissions?.write) && !r.mirror;
  const resolveRelative = (path: string, k: "link" | "image") =>
    k === "image" ? resolveUrl(kind, ns, name, rev, path) : blobPath(kind, ns, name, rev, path);

  if (editing && kind === "dataset") {
    return (
      <CardEditor
        repo={r}
        data={cardData}
        body={split.body}
        parentCommit={r.sha}
        onDone={() => setEditing(false)}
      />
    );
  }
  return (
    <div className="grid gap-8 lg:grid-cols-[minmax(0,1fr)_300px]">
      <div className="min-w-0">
        {canWrite && readme.data !== null && readme.data !== undefined ? (
          <div className="mb-3 flex justify-end">
            {kind === "dataset" ? (
              <Button size="sm" onClick={() => setEditing(true)}>
                <Pencil aria-hidden="true" />
                Edit card
              </Button>
            ) : (
              <Button asChild size="sm">
                <AppLink href={`${blobPath(kind, ns, name, rev, "README.md")}?edit=1`}>
                  <Pencil aria-hidden="true" />
                  Edit card
                </AppLink>
              </Button>
            )}
          </div>
        ) : null}
        {yamlError ? (
          <p
            role="alert"
            className="mb-3 rounded-md border border-warn/30 bg-warn-soft px-3 py-2 text-sm text-warn"
          >
            The card's YAML metadata is not valid, so Hangar ignores it: {yamlError}
          </p>
        ) : null}
        {readme.isPending ? (
          <div className="space-y-2" role="status" aria-label="Loading the card">
            <Skeleton className="h-8 w-1/2" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-5/6" />
          </div>
        ) : readme.isError ? (
          <ErrorState error={readme.error} onRetry={() => readme.refetch()} />
        ) : readme.data === null ? (
          <EmptyState
            icon={<FilePlus />}
            title={
              kind === "dataset"
                ? "This dataset has no card yet"
                : "This model has no Model card yet"
            }
            action={
              canWrite ? (
                kind === "dataset" ? (
                  <Button variant="primary" onClick={() => setEditing(true)}>
                    Create a dataset card
                  </Button>
                ) : (
                  <Button asChild variant="primary">
                    <AppLink href={`${blobPath(kind, ns, name, rev, "README.md")}?edit=1`}>
                      Create a Model card
                    </AppLink>
                  </Button>
                )
              ) : null
            }
          >
            A card is the Repo's <code className="font-mono">README.md</code>. It tells people what
            this is and how to use it.
          </EmptyState>
        ) : (
          <LazyMarkdown externalImages={externalImages} resolveRelative={resolveRelative}>
            {split.body}
          </LazyMarkdown>
        )}
      </div>
      <Sidebar repo={r} cardData={cardData} />
    </div>
  );
}

export { repoKey, repoPathParams };
