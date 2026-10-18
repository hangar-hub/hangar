import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { File, FilePlus, Folder, History, Upload } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Repo, TreeEntry } from "@/api/types";
import { AppLink, useGo } from "@/components/AppLink";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Textarea } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { type Column, DataTable } from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatDateTime, relativeTime } from "@/lib/format";
import { errorMessage, unwrap } from "@/lib/problem";
import { baseName, blobPath, joinPath, type RepoKind, repoPath, treePath } from "@/lib/repo";
import { NotPresentBadge, ScanBadge } from "../repos/badges";
import { type RepoRef, repoKey, repoPathParams, useRepo } from "../repos/hooks";
import { UploadPanel } from "../upload/UploadPanel";
import { Breadcrumbs, RefPicker, ZipButton } from "./common";

function toBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function NewFileDialog({ repo, branch, folder }: { repo: Repo; branch: string; folder: string }) {
  const [open, setOpen] = useState(false);
  const [path, setPath] = useState(folder ? `${folder}/` : "");
  const [content, setContent] = useState("");
  const [summary, setSummary] = useState("");
  const qc = useQueryClient();
  const go = useGo();
  const ref = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/commits", {
          params: { path: repoPathParams(ref) },
          body: {
            summary: summary || `Create ${joinPath(path)}`,
            branch,
            operations: [{ op: "add", path: joinPath(path), content_base64: toBase64(content) }],
          },
        }),
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: repoKey(ref) });
      setOpen(false);
      go(blobPath(repo.type, repo.namespace, repo.name, branch, joinPath(path)));
    },
  });
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      wide
      title="New file"
      description={`Commits a new text file to ${branch}.`}
      trigger={
        <Button size="sm">
          <FilePlus aria-hidden="true" />
          New file
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            form="new-file"
            type="submit"
            disabled={!joinPath(path) || create.isPending}
          >
            Commit new file
          </Button>
        </>
      }
    >
      <form
        id="new-file"
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="File path">
          {(p) => (
            <Input
              {...p}
              className="font-mono"
              value={path}
              onChange={(e) => setPath(e.target.value)}
              required
            />
          )}
        </Field>
        <Field label="Content">
          {(p) => (
            <Textarea
              {...p}
              rows={12}
              className="font-mono text-[13px]"
              value={content}
              onChange={(e) => setContent(e.target.value)}
            />
          )}
        </Field>
        <Field label="Commit message">
          {(p) => (
            <Input
              {...p}
              value={summary}
              placeholder={`Create ${joinPath(path) || "file"}`}
              onChange={(e) => setSummary(e.target.value)}
            />
          )}
        </Field>
        <InlineError error={create.error} />
      </form>
    </Dialog>
  );
}

function PullFileButton({ repo, path }: { repo: Repo; path: string }) {
  const toast = useToast();
  const qc = useQueryClient();
  const pull = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/mirror/pull", {
          params: {
            path: repoPathParams({ kind: repo.type, ns: repo.namespace, name: repo.name }),
          },
          body: { paths: [path] },
        }),
      ),
    onSuccess: () => {
      toast(`Pulling ${baseName(path)}. Follow it in Jobs.`);
      qc.invalidateQueries({ queryKey: ["jobs"] });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  return (
    <Button
      size="sm"
      variant="link"
      onClick={() => pull.mutate()}
      disabled={pull.isPending || pull.isSuccess}
    >
      {pull.isSuccess ? "Pull started" : "Pull"}
    </Button>
  );
}

/** The Files tab: a folder at a Revision with presence, LFS and scan badges (BROWSE-3, PULL-3, DOWN-5, UP-3). */
export function TreePage({
  kind,
  ns,
  name,
  rev,
  path,
}: {
  kind: RepoKind;
  ns: string;
  name: string;
  rev: string;
  path: string;
}) {
  const ref: RepoRef = { kind, ns, name };
  const repo = useRepo(ref);
  const [uploading, setUploading] = useState(false);
  const dir = path.replace(/\/$/, "");
  const tree = useInfiniteQuery({
    queryKey: [...repoKey(ref), "tree", rev, dir],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) => {
      const query = { limit: 500, ...(pageParam ? { cursor: pageParam } : {}) };
      return unwrap(
        dir
          ? await api.GET("/repos/{type}/{ns}/{name}/tree/{rev}/{path}", {
              params: { path: { ...repoPathParams(ref), rev, path: dir }, query },
            })
          : await api.GET("/repos/{type}/{ns}/{name}/tree/{rev}", {
              params: { path: { ...repoPathParams(ref), rev }, query },
            }),
      );
    },
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const r = repo.data;
  if (!r) return null;
  const entries = tree.data?.pages.flatMap((p) => p.items) ?? [];
  const isFile = entries.length === 1 && entries[0]?.type === "file" && entries[0]?.path === dir;
  const canWrite = Boolean(r.permissions?.write) && !r.mirror;
  const canPull = r.mirror && Boolean(r.permissions?.pull_mirror);
  const commit = tree.data?.pages[0]?.commit;
  const columns: Column<TreeEntry>[] = [
    {
      key: "name",
      header: "Name",
      primary: true,
      cell: (e) => {
        const n = baseName(e.path);
        const href =
          e.type === "directory"
            ? treePath(kind, ns, name, rev, e.path)
            : blobPath(kind, ns, name, rev, e.path);
        return (
          <span className="flex min-w-0 flex-wrap items-center gap-1.5">
            {e.type === "directory" ? (
              <Folder className="size-4 shrink-0 text-accent" aria-label="Folder" />
            ) : (
              <File className="size-4 shrink-0 text-muted" aria-label="File" />
            )}
            <AppLink href={href} className="break-all font-mono text-[13px] hover:underline">
              {n}
            </AppLink>
            {e.lfs ? <Badge mono>LFS</Badge> : null}
            {!e.present ? <NotPresentBadge /> : null}
            {!e.present && canPull && e.type === "file" ? (
              <PullFileButton repo={r} path={e.path} />
            ) : null}
            <ScanBadge status={e.scan_status} hideSafe />
          </span>
        );
      },
    },
    {
      key: "size",
      header: "Size",
      className: "w-28 text-right max-md:text-left",
      cell: (e) => (
        <span className="tabular font-mono text-xs text-muted">{formatBytes(e.size)}</span>
      ),
    },
    {
      key: "commit",
      header: "Last Commit",
      cell: (e) =>
        e.last_commit ? (
          <AppLink
            href={`${repoPath(kind, ns, name)}/commit/${e.last_commit.sha}`}
            title={e.last_commit.title}
            className="line-clamp-1 text-xs text-muted transition-colors duration-100 hover:text-ink hover:underline"
          >
            {e.last_commit.title}
          </AppLink>
        ) : null,
    },
    {
      key: "date",
      header: "Updated",
      className: "w-32",
      cell: (e) =>
        e.last_commit ? (
          <time
            dateTime={e.last_commit.date}
            title={formatDateTime(e.last_commit.date)}
            className="text-xs whitespace-nowrap text-muted"
          >
            {relativeTime(e.last_commit.date)}
          </time>
        ) : null,
    },
  ];
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <RefPicker repo={ref} rev={rev} build={(v) => treePath(kind, ns, name, v, dir)} />
        <Breadcrumbs kind={kind} ns={ns} name={name} rev={rev} path={dir} />
        <div className="ml-auto flex flex-wrap gap-2">
          <Button asChild size="sm" variant="ghost">
            <AppLink
              href={`${repoPath(kind, ns, name)}/commits/${encodeURIComponent(rev)}${dir ? `?path=${encodeURIComponent(dir)}` : ""}`}
            >
              <History aria-hidden="true" />
              History
            </AppLink>
          </Button>
          <ZipButton repo={ref} rev={commit ?? rev} path={dir || undefined} />
          {canWrite ? (
            <>
              <NewFileDialog repo={r} branch={rev} folder={dir} />
              <Button
                size="sm"
                variant="primary"
                onClick={() => setUploading(true)}
                aria-expanded={uploading}
              >
                <Upload aria-hidden="true" />
                Upload files
              </Button>
            </>
          ) : null}
        </div>
      </div>
      {uploading ? (
        <UploadPanel repo={r} branch={rev} folder={dir} onClose={() => setUploading(false)} />
      ) : null}
      {tree.isPending ? (
        <LoadingRows rows={6} label="Loading files" />
      ) : tree.isError ? (
        <ErrorState error={tree.error} onRetry={() => tree.refetch()} />
      ) : isFile ? (
        <EmptyState
          title={`${dir} is a file`}
          action={
            <Button asChild variant="primary">
              <AppLink href={blobPath(kind, ns, name, rev, dir)}>Open the file</AppLink>
            </Button>
          }
        />
      ) : entries.length === 0 ? (
        <EmptyState
          icon={<Upload />}
          title="No files yet"
          action={
            canWrite ? (
              <Button variant="primary" onClick={() => setUploading(true)}>
                Upload files
              </Button>
            ) : null
          }
        >
          Upload files here, push with git, or use <code className="font-mono">hf upload</code>.
        </EmptyState>
      ) : (
        <>
          <DataTable
            caption={`Files in ${dir || "the Repo root"}`}
            className="hg-enter"
            rowHover
            columns={columns}
            rows={entries}
            rowKey={(e) => e.path}
          />
          {tree.hasNextPage ? (
            <Button onClick={() => tree.fetchNextPage()} disabled={tree.isFetchingNextPage}>
              Show more files
            </Button>
          ) : null}
        </>
      )}
    </div>
  );
}
