import { useInfiniteQuery } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { api } from "@/api/client";
import { AppLink } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { formatDateTime, relativeTime, shortSha } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { blobPath, type RepoKind, repoPath } from "@/lib/repo";
import { type RepoRef, repoKey, repoPathParams } from "../repos/hooks";
import { RefPicker } from "./common";

/** Commits on a Revision, newest first, optionally for one path (REPO-3). */
export function CommitsPage({
  kind,
  ns,
  name,
  rev,
}: {
  kind: RepoKind;
  ns: string;
  name: string;
  rev: string;
}) {
  const ref: RepoRef = { kind, ns, name };
  const { path } = useSearch({ strict: false }) as { path?: string };
  const list = useInfiniteQuery({
    queryKey: [...repoKey(ref), "commits", rev, path ?? ""],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/commits", {
          params: {
            path: repoPathParams(ref),
            query: { rev, ...(path ? { path } : {}), ...(pageParam ? { cursor: pageParam } : {}) },
          },
        }),
      ),
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  const base = repoPath(kind, ns, name);
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <RefPicker
          repo={ref}
          rev={rev}
          build={(v) =>
            `${base}/commits/${encodeURIComponent(v)}${path ? `?path=${encodeURIComponent(path)}` : ""}`
          }
        />
        <h2 className="text-sm">
          Commits
          {path ? (
            <>
              {" "}
              to{" "}
              <AppLink
                href={blobPath(kind, ns, name, rev, path)}
                className="font-mono text-accent hover:underline"
              >
                {path}
              </AppLink>
            </>
          ) : null}
        </h2>
      </div>
      {list.isPending ? (
        <LoadingRows rows={6} label="Loading Commits" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState title="No Commits">
          {path ? "No Commit on this Revision changes this path." : "This Revision has no Commits."}
        </EmptyState>
      ) : (
        <ol
          aria-label="Commits"
          className="divide-y divide-line rounded-lg border border-line bg-surface"
        >
          {items.map((c) => (
            <li key={c.sha} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-3">
              <div className="min-w-0 flex-1">
                <AppLink href={`${base}/commit/${c.sha}`} className="font-medium hover:underline">
                  {c.title}
                </AppLink>
                <p className="mt-0.5 text-xs text-muted">
                  {c.author.username ?? c.author.name} committed{" "}
                  <time dateTime={c.author.date} title={formatDateTime(c.author.date)}>
                    {relativeTime(c.author.date)}
                  </time>
                </p>
              </div>
              <span className="flex items-center gap-1">
                <AppLink
                  href={`${base}/commit/${c.sha}`}
                  className="rounded-sm border border-line px-1.5 font-mono text-xs hover:bg-raised"
                >
                  {shortSha(c.sha)}
                </AppLink>
                <CopyButton value={c.sha} label={`Copy the SHA of ${shortSha(c.sha)}`} />
              </span>
            </li>
          ))}
        </ol>
      )}
      {list.hasNextPage ? (
        <Button onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
          Older Commits
        </Button>
      ) : null}
    </div>
  );
}
