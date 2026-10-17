import { Database, Download, Heart } from "lucide-react";
import type { Repo } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { Badge } from "@/components/ui/badge";
import { formatBytes, formatCompact, formatParams, relativeTime } from "@/lib/format";
import { repoLabel, repoPath } from "@/lib/repo";
import { cn } from "@/lib/utils";
import { FormatBadges, MirrorBadge, QuantChips, ScanBadge } from "./badges";

/** Size present out of total, for Partial mirrors; just the size otherwise. */
export function sizeText(
  r: Pick<Repo, "size_bytes" | "lfs_bytes" | "present_bytes" | "mirror">,
): string {
  const total = r.size_bytes;
  if (r.mirror && r.present_bytes !== undefined && r.present_bytes < total) {
    return `${formatBytes(r.present_bytes)} of ${formatBytes(total)}`;
  }
  return formatBytes(total);
}

/** Quant chips a grid card shows before the "+N" chip. */
export const cardQuants = 3;

/**
 * A Repo in the Models or Datasets list: ID, format badges, parameters,
 * quant chips, size present of total, Mirror and scan badges, and when it
 * changed (09-web-ui.md "/models and /datasets"). In a grid the card fills
 * its cell, so cards in a row are the same height, with the stats row at
 * the bottom.
 */
export function RepoCard({ repo, layout = "grid" }: { repo: Repo; layout?: "grid" | "list" }) {
  const s = repo.summary ?? {};
  return (
    <article
      className={cn(
        "group relative min-w-0 rounded-lg border border-line bg-surface p-3.5 hover:border-line-strong",
        layout === "grid" && "flex h-full flex-col",
        layout === "list" && "sm:flex sm:items-center sm:gap-4",
      )}
    >
      <div className="min-w-0 flex-1">
        <h3 className="flex items-center gap-1.5 font-mono text-sm font-medium">
          {repo.type === "dataset" ? (
            <Database className="size-4 shrink-0 text-muted" aria-label="Dataset" />
          ) : null}
          <AppLink
            href={repoPath(repo.type, repo.namespace, repo.name)}
            className="truncate after:absolute after:inset-0 hover:underline"
          >
            {repoLabel(repo.type, repo.namespace, repo.name)}
          </AppLink>
        </h3>
        <div className="mt-2 flex flex-wrap gap-1">
          {s.parameters ? <Badge mono>{formatParams(s.parameters)}</Badge> : null}
          <FormatBadges formats={s.formats} />
          <QuantChips quants={s.quants} max={cardQuants} />
          {repo.mirror ? (
            <MirrorBadge
              upstream={repo.mirror_status?.upstream}
              url={repo.mirror_status?.upstream_url}
            />
          ) : null}
          <ScanBadge status={s.scan_status} hideSafe />
        </div>
      </div>
      <dl
        className={cn(
          "tabular mt-3 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted",
          layout === "list" && "sm:mt-0 sm:shrink-0",
        )}
      >
        {s.pipeline_tag ? (
          <div>
            <dt className="sr-only">Task</dt>
            <dd>{s.pipeline_tag}</dd>
          </div>
        ) : null}
        <div>
          <dt className="sr-only">Size</dt>
          <dd className="font-mono">{sizeText(repo)}</dd>
        </div>
        <div className="flex items-center gap-1">
          <dt>
            <Download className="size-3" aria-label="Downloads" />
          </dt>
          <dd>{formatCompact(repo.downloads)}</dd>
        </div>
        <div className="flex items-center gap-1">
          <dt>
            <Heart className="size-3" aria-label="Likes" />
          </dt>
          <dd>{formatCompact(repo.likes)}</dd>
        </div>
        <div>
          <dt className="sr-only">Updated</dt>
          <dd>Updated {relativeTime(repo.last_modified ?? repo.updated_at)}</dd>
        </div>
      </dl>
    </article>
  );
}
