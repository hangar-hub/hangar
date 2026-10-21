import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { Boxes, Database, Heart, RefreshCw, Unlink } from "lucide-react";
import { api } from "@/api/client";
import type { Repo } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { useMyJobs } from "@/components/JobsDrawer";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyButton } from "@/components/ui/copy-button";
import { ProgressBar } from "@/components/ui/progress";
import { EmptyState, ErrorState, Skeleton } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import {
  formatBytes,
  formatCompact,
  formatDuration,
  formatRate,
  relativeTime,
  shortSha,
} from "@/lib/format";
import { isActive } from "@/lib/jobs";
import { useDocumentTitle } from "@/lib/page";
import { errorMessage, errorStatus, unwrap } from "@/lib/problem";
import { baseName, type RepoKind, repoPath } from "@/lib/repo";
import { useMe } from "@/lib/session";
import { cn } from "@/lib/utils";
import {
  GatedBadge,
  MirrorBadge,
  ScanBadge,
  UpdateAvailableBadge,
  VisibilityBadge,
} from "./badges";
import { type RepoRef, repoKey, repoPathParams, useRepo } from "./hooks";
import { UsePanel } from "./UsePanel";

function LikeButton({ repo }: { repo: Repo }) {
  const qc = useQueryClient();
  const ref = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const like = useMutation({
    mutationFn: async () => {
      const path = { params: { path: repoPathParams(ref) } };
      return unwrap(
        repo.liked
          ? await api.DELETE("/repos/{type}/{ns}/{name}/like", path)
          : await api.POST("/repos/{type}/{ns}/{name}/like", path),
      );
    },
    onSuccess: (s) => qc.setQueryData(repoKey(ref), { ...repo, liked: s.liked, likes: s.likes }),
  });
  return (
    <Button
      size="sm"
      aria-pressed={Boolean(repo.liked)}
      onClick={() => like.mutate()}
      disabled={like.isPending}
    >
      <Heart aria-hidden="true" className={cn(repo.liked && "fill-current text-danger")} />
      {repo.liked ? "Liked" : "Like"}
      <span className="tabular font-mono text-xs text-muted">{formatCompact(repo.likes)}</span>
    </Button>
  );
}

/** Mirror status: the Upstream, last sync, live Pull progress, and Sync now and Detach. */
function MirrorStatus({ repo }: { repo: Repo }) {
  const qc = useQueryClient();
  const toast = useToast();
  const ref = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const jobs = useMyJobs(true);
  const active =
    jobs.data?.items.filter((j) => isActive(j.status) && j.payload?.repo === repo.id) ?? [];
  const sync = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/mirror/sync", {
          params: { path: repoPathParams(ref) },
        }),
      ),
    onSuccess: () => {
      toast("Sync started.");
      qc.invalidateQueries({ queryKey: ["jobs"] });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const m = repo.mirror_status;
  const canAdmin = repo.permissions?.admin;
  return (
    <div className="mt-3 space-y-2 rounded-lg border border-info/30 bg-info-soft/50 px-3 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <MirrorBadge upstream={m?.upstream} url={m?.upstream_url} />
        {m?.upstream_url ? (
          <a
            href={m.upstream_url}
            className="font-mono text-xs text-info underline"
            rel="noopener noreferrer"
            target="_blank"
          >
            {m.upstream_repo}
          </a>
        ) : null}
        <span className="text-xs text-muted">
          {m?.last_synced_at ? `Last synced ${relativeTime(m.last_synced_at)}` : "Not synced yet"}
        </span>
        {m?.update_available ? <UpdateAvailableBadge /> : null}
        {canAdmin || repo.permissions?.pull_mirror ? (
          <span className="ml-auto flex gap-1">
            <Button size="sm" onClick={() => sync.mutate()} disabled={sync.isPending}>
              <RefreshCw aria-hidden="true" />
              Sync now
            </Button>
            {canAdmin ? (
              <ConfirmDialog
                trigger={
                  <Button size="sm" variant="ghost">
                    <Unlink aria-hidden="true" />
                    Detach
                  </Button>
                }
                title="Detach this Mirror?"
                actionLabel="Detach"
                onConfirm={async () => {
                  const r = unwrap(
                    await api.POST("/repos/{type}/{ns}/{name}/mirror/detach", {
                      params: { path: repoPathParams(ref) },
                    }),
                  );
                  qc.setQueryData(repoKey(ref), r);
                  toast("Detached. This is now an ordinary Repo.");
                }}
              >
                <p>
                  The Repo keeps its history but stops following{" "}
                  {m?.upstream_repo ?? "the Upstream"}. You can push to it and upload files
                  afterwards. Clients that use it through{" "}
                  <code className="font-mono">HF_ENDPOINT</code> keep working.
                </p>
              </ConfirmDialog>
            ) : null}
          </span>
        ) : null}
      </div>
      {active.map((j) => (
        <div key={j.id} className="space-y-1">
          <ProgressBar
            done={j.progress.bytes_done}
            total={j.progress.bytes_total}
            label={`Pull progress for ${repo.id}`}
          />
          <p className="tabular font-mono text-xs text-muted">
            Pulling {formatBytes(j.progress.bytes_done)} of {formatBytes(j.progress.bytes_total)},{" "}
            {formatRate(j.progress.rate)}, {formatDuration(j.progress.eta_seconds)} left
          </p>
        </div>
      ))}
    </div>
  );
}

type Tab = { to: string; label: string; match: RegExp; show: boolean };

/**
 * The browser tab's text for a Repo page, before "· Hangar": the Repo ID,
 * after the file, Commit, discussion or tab when the page shows one.
 * `rest` is the path after the Repo's own, such as "/tree/main/data";
 * `params` are the route's, which hold the file path decoded.
 */
export function repoPageTitle(
  id: string,
  kind: RepoKind,
  rest: string,
  params: { _splat?: string; sha?: string; num?: string },
): string {
  const section = /^\/([^/]+)/.exec(rest)?.[1] ?? "";
  let page = "";
  switch (section) {
    case "tree":
    case "blob":
      page = params._splat ? baseName(params._splat.replace(/\/$/, "")) : "Files";
      break;
    case "commits":
      page = "Commits";
      break;
    case "commit":
      page = params.sha ? `Commit ${shortSha(params.sha)}` : "Commits";
      break;
    case "discussions":
      page = params.num ? `Discussion #${params.num}` : "Community";
      break;
    case "viewer":
      page = "Viewer";
      break;
    case "settings":
      page = "Settings";
      break;
    default:
      page = "";
  }
  const repo = kind === "dataset" ? `datasets/${id}` : id;
  return page ? `${page} · ${repo}` : repo;
}

/** Header, tabs and page content shared by every Repo page. */
export function RepoLayout({ kind, ns, name }: { kind: RepoKind; ns: string; name: string }) {
  const ref: RepoRef = { kind, ns, name };
  const repo = useRepo(ref);
  const me = useMe();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const params = useRouterState({
    select: (s) => s.matches[s.matches.length - 1]?.params as Record<string, string> | undefined,
  });
  const base = repoPath(kind, ns, name);
  const rest = decodeURIComponent(pathname)
    .slice(decodeURIComponent(base).length)
    .replace(/\/$/, "");
  useDocumentTitle(repoPageTitle(`${ns}/${name}`, kind, rest, params ?? {}));

  if (repo.isPending) {
    return (
      <div className="space-y-3" role="status" aria-label="Loading the Repo">
        <Skeleton className="h-8 w-80" />
        <Skeleton className="h-6 w-56" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }
  if (repo.isError) {
    const status = errorStatus(repo.error);
    if (status === 404 || status === 401) {
      return (
        <section aria-labelledby="page-title" className="space-y-4">
          <h1 id="page-title" className="font-mono text-xl font-semibold break-all">
            {kind === "dataset" ? "datasets/" : ""}
            {ns}/{name}
          </h1>
          <EmptyState
            title="This Repo doesn't exist, or you can't see it"
            action={
              !me.data ? (
                <Button asChild variant="primary">
                  <Link to="/auth/login" search={{ redirect: pathname }}>
                    Sign in
                  </Link>
                </Button>
              ) : (
                <Button asChild>
                  <Link to="/discover" search={{ q: `${ns}/${name}` }}>
                    Look for it on an Upstream
                  </Link>
                </Button>
              )
            }
          >
            Check the ID. Private Repos are only visible to members of their Namespace and
            collaborators.
          </EmptyState>
        </section>
      );
    }
    return <ErrorState error={repo.error} onRetry={() => repo.refetch()} />;
  }
  const r = repo.data;
  const tabs: Tab[] = [
    {
      to: base,
      label: kind === "dataset" ? "Dataset card" : "Model card",
      match: /^$/,
      show: true,
    },
    { to: `${base}/viewer`, label: "Viewer", match: /^\/viewer/, show: kind === "dataset" },
    {
      to: `${base}/tree/${encodeURIComponent(r.default_branch)}`,
      label: "Files",
      match: /^\/(tree|blob)\//,
      show: true,
    },
    {
      to: `${base}/commits/${encodeURIComponent(r.default_branch)}`,
      label: "Commits",
      match: /^\/(commits?)\//,
      show: true,
    },
    { to: `${base}/discussions`, label: "Community", match: /^\/discussions/, show: true },
    {
      to: `${base}/settings`,
      label: "Settings",
      match: /^\/settings/,
      show: Boolean(r.permissions?.admin),
    },
  ];
  return (
    <div className="space-y-5">
      <header>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="flex items-center gap-1.5 text-xs text-muted">
              {kind === "dataset" ? (
                <Database className="size-3.5" aria-hidden="true" />
              ) : (
                <Boxes className="size-3.5" aria-hidden="true" />
              )}
              {kind === "dataset" ? "Dataset" : "Model"}
            </p>
            <h1
              id="page-title"
              className="mt-0.5 flex flex-wrap items-center gap-x-1 font-mono text-xl font-semibold tracking-tight"
            >
              <Link
                to="/$ns"
                params={{ ns: r.namespace }}
                className="text-muted transition-[color] duration-150 hover:text-ink hover:underline"
              >
                {r.namespace}
              </Link>
              <span className="text-muted" aria-hidden="true">
                /
              </span>
              <span className="break-all">{r.name}</span>
              <CopyButton value={r.id} label="Copy the Repo ID" />
            </h1>
            <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
              <VisibilityBadge visibility={r.visibility} />
              {r.gated ? <GatedBadge /> : null}
              <ScanBadge status={r.summary?.scan_status} />
              {r.licence ? <Badge>{r.licence}</Badge> : null}
              {r.summary?.pipeline_tag ? <Badge>{r.summary.pipeline_tag}</Badge> : null}
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            {me.data ? <LikeButton repo={r} /> : null}
            <UsePanel repo={r} />
          </div>
        </div>
        {r.mirror ? <MirrorStatus repo={r} /> : null}
        {r.deleted_at ? (
          <p
            role="status"
            className="mt-3 rounded-md border border-danger/30 bg-danger-soft px-3 py-2 text-sm text-danger"
          >
            This Repo was deleted and can be restored from its settings until it is purged.
          </p>
        ) : null}
        <nav aria-label="Repo" className="mt-4 flex gap-1 overflow-x-auto border-b border-line">
          {tabs
            .filter((t) => t.show)
            .map((t) => {
              const active = t.match.test(rest);
              return (
                <AppLink
                  key={t.label}
                  href={t.to}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "-mb-px whitespace-nowrap border-b-2 border-transparent px-3 py-2 text-sm text-muted transition-[color,border-color] duration-150 hover:text-ink",
                    active && "border-accent-fill font-medium text-ink",
                  )}
                >
                  {t.label}
                </AppLink>
              );
            })}
        </nav>
      </header>
      <Outlet />
    </div>
  );
}
