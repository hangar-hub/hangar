import { useQuery } from "@tanstack/react-query";
import { api } from "@/api/client";
import { AppLink } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { formatDateTime, shortSha } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { type RepoKind, repoPath, treePath } from "@/lib/repo";
import { type RepoRef, repoKey, repoPathParams } from "../repos/hooks";
import { ChangeList } from "./ChangeList";

/** One Commit with its file-level diff (REPO-3). */
export function CommitPage({
  kind,
  ns,
  name,
  sha,
}: {
  kind: RepoKind;
  ns: string;
  name: string;
  sha: string;
}) {
  const ref: RepoRef = { kind, ns, name };
  const commit = useQuery({
    queryKey: [...repoKey(ref), "commit", sha],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/commits/{sha}", {
          params: { path: { ...repoPathParams(ref), sha } },
        }),
      ),
  });
  if (commit.isPending) return <LoadingRows rows={4} label="Loading the Commit" />;
  if (commit.isError) return <ErrorState error={commit.error} onRetry={() => commit.refetch()} />;
  const c = commit.data;
  const body = c.message.slice(c.title.length).trim();
  return (
    <div className="space-y-4">
      <section
        aria-labelledby="commit-title"
        className="hg-enter rounded-lg border border-line bg-surface p-4"
      >
        <h2 id="commit-title" className="text-lg font-semibold wrap-anywhere">
          {c.title}
        </h2>
        {body ? <p className="mt-2 whitespace-pre-wrap text-sm text-muted">{body}</p> : null}
        <dl className="mt-3 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[max-content_1fr]">
          <dt className="text-muted">Author</dt>
          <dd className="wrap-anywhere">
            {c.author.name} <span className="text-muted">&lt;{c.author.email}&gt;</span>,{" "}
            <time dateTime={c.author.date} className="tabular">
              {formatDateTime(c.author.date)}
            </time>
          </dd>
          <dt className="text-muted">Commit</dt>
          <dd className="flex min-w-0 items-center gap-1 font-mono text-xs">
            <span className="min-w-0 break-all">{c.sha}</span>
            <CopyButton value={c.sha} label="Copy the Commit SHA" />
          </dd>
          {c.parents.length ? (
            <>
              <dt className="text-muted">{c.parents.length > 1 ? "Parents" : "Parent"}</dt>
              <dd className="flex flex-wrap gap-2 font-mono text-xs">
                {c.parents.map((p) => (
                  <AppLink
                    key={p}
                    href={`${repoPath(kind, ns, name)}/commit/${p}`}
                    title={p}
                    className="hg-link"
                  >
                    {shortSha(p)}
                  </AppLink>
                ))}
              </dd>
            </>
          ) : null}
        </dl>
        <Button asChild size="sm" className="mt-3">
          <AppLink href={treePath(kind, ns, name, c.sha)}>Browse files at this Commit</AppLink>
        </Button>
      </section>
      <h3 className="tabular text-sm font-semibold">
        {c.changes.length} {c.changes.length === 1 ? "file" : "files"} changed
      </h3>
      {c.changes.length ? (
        <ChangeList repo={ref} sha={c.sha} changes={c.changes} />
      ) : (
        <EmptyState size="sm" title="No file changes">This Commit changes no files.</EmptyState>
      )}
    </div>
  );
}
