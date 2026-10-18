import { useQuery } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { FileChange } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { ErrorState, Skeleton } from "@/components/ui/states";
import { formatBytes, shortSha } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { cn } from "@/lib/utils";
import { type RepoRef, repoKey, repoPathParams } from "../repos/hooks";
import { DiffView } from "./DiffView";

const statusTone = { added: "ok", modified: "info", deleted: "danger", renamed: "accent" } as const;
const statusText = {
  added: "Added",
  modified: "Modified",
  deleted: "Deleted",
  renamed: "Renamed",
} as const;

function Diff({
  repo,
  sha,
  pullRequest,
  change,
}: {
  repo: RepoRef;
  sha: string;
  pullRequest?: number;
  change: FileChange;
}) {
  const diff = useQuery({
    queryKey: [...repoKey(repo), "diff", pullRequest ? `pr-${pullRequest}` : sha, change.path],
    queryFn: async () =>
      unwrap(
        pullRequest
          ? await api.GET("/repos/{type}/{ns}/{name}/discussions/{num}/diff", {
              params: {
                path: { ...repoPathParams(repo), num: pullRequest },
                query: { path: change.path },
              },
            })
          : await api.GET("/repos/{type}/{ns}/{name}/commits/{sha}/diff", {
              params: { path: { ...repoPathParams(repo), sha }, query: { path: change.path } },
            }),
      ),
  });
  if (diff.isPending) return <Skeleton className="m-3 h-20" />;
  if (diff.isError) return <ErrorState error={diff.error} onRetry={() => diff.refetch()} />;
  if (!diff.data.patch) return <SizeChange change={change} />;
  return (
    <div className="hg-enter">
      <DiffView patch={diff.data.patch} label={`Diff of ${change.path}`} />
    </div>
  );
}

/** For LFS, binary and large files: the size and SHA change instead of a text diff (REPO-3). */
function SizeChange({ change }: { change: FileChange }) {
  return (
    <dl className="grid gap-x-4 gap-y-1 px-4 py-3 font-mono text-xs sm:grid-cols-[max-content_1fr]">
      {change.old_oid ? (
        <>
          <dt className="text-muted">Before</dt>
          <dd>
            <span className="tabular">{formatBytes(change.old_size)}</span>,{" "}
            {change.lfs ? "SHA-256" : "object"}{" "}
            <span title={change.old_oid}>{shortSha(change.old_oid)}</span>
          </dd>
        </>
      ) : null}
      {change.new_oid ? (
        <>
          <dt className="text-muted">After</dt>
          <dd>
            <span className="tabular">{formatBytes(change.new_size)}</span>,{" "}
            {change.lfs ? "SHA-256" : "object"}{" "}
            <span title={change.new_oid}>{shortSha(change.new_oid)}</span>
          </dd>
        </>
      ) : null}
    </dl>
  );
}

/**
 * Files changed in a Commit or pull request, each with an expandable diff.
 * For a pull request, pass its number: each diff then covers every Commit
 * of the pull request, from where it left its base branch.
 */
export function ChangeList({
  repo,
  sha,
  pullRequest,
  changes,
}: {
  repo: RepoRef;
  sha: string;
  pullRequest?: number;
  changes: FileChange[];
}) {
  const [open, setOpen] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(changes.slice(0, 5).map((c) => [c.path, true])),
  );
  return (
    <ul aria-label="Changed files" className="space-y-2">
      {changes.map((c) => {
        const isOpen = Boolean(open[c.path]);
        return (
          <li key={c.path} className="overflow-hidden rounded-lg border border-line bg-surface">
            <button
              type="button"
              aria-expanded={isOpen}
              onClick={() => setOpen({ ...open, [c.path]: !isOpen })}
              className={cn(
                "flex w-full flex-wrap items-center gap-2 px-3 py-2 text-left transition-colors duration-100 hover:bg-raised",
                isOpen && "border-b border-line",
              )}
            >
              <ChevronRight
                className={cn("size-4 shrink-0 text-muted", isOpen && "rotate-90")}
                aria-hidden="true"
              />
              <Badge tone={statusTone[c.status]}>{statusText[c.status]}</Badge>
              <span className="min-w-0 flex-1 break-all font-mono text-[13px]">
                {c.old_path && c.old_path !== c.path ? `${c.old_path} to ${c.path}` : c.path}
              </span>
              {c.lfs ? <Badge mono>LFS</Badge> : null}
              {c.binary && !c.lfs ? <Badge>Binary</Badge> : null}
            </button>
            {isOpen ? (
              c.lfs || c.binary ? (
                <SizeChange change={c} />
              ) : (
                <Diff repo={repo} sha={sha} pullRequest={pullRequest} change={c} />
              )
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
