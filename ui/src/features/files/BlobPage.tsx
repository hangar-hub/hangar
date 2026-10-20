import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { Download, FileCode, History, Link2, Pencil, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { ScanFinding } from "@/api/types";
import { AppLink, useGo } from "@/components/AppLink";
import { CodeView } from "@/components/content/CodeView";
import { LazyMarkdown } from "@/components/content/LazyMarkdown";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Field, Input, Textarea } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { Tabs } from "@/components/ui/tabs";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatDateTime, relativeTime, shortSha } from "@/lib/format";
import { splitFrontMatter } from "@/lib/frontmatter";
import { errorStatus, unwrap } from "@/lib/problem";
import {
  baseName,
  blobPath,
  dirName,
  fileView,
  languageOf,
  type RepoKind,
  repoPath,
  resolveUrl,
} from "@/lib/repo";
import { LfsBadge, NotPresentBadge, ScanBadge, type ScanStatus } from "../repos/badges";
import { fetchText, type RepoRef, repoKey, repoPathParams, useRefs, useRepo } from "../repos/hooks";
import { useExternalImages } from "../repos/useInstance";
import { Breadcrumbs, RefPicker } from "./common";
import { RiskyDownload } from "./RiskyDownload";
import { GgufView, JsonTree, SafetensorsView } from "./viewers";

/** Text files up to this size open in the browser; larger ones are download-only. */
const maxPreview = 5 * 1024 * 1024;
/** Files up to this size can be edited in the browser (09-web-ui.md). */
const maxEdit = 1024 * 1024;

const pickleExtensions = /\.(bin|pt|pth|ckpt|pkl|pickle|joblib|npy)$/i;

function toBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

const scanExplanations: Record<ScanStatus, string> = {
  safe: "The pickle scan found only imports that rebuild tensors and containers.",
  suspicious:
    "The pickle scan found imports that are not on the allowlist. Loading this file with pickle could run them. Check the source before you load it, or prefer a safetensors copy.",
  unsafe:
    "The pickle scan found imports that can run code or touch the system, such as os or subprocess. Do not load this file with pickle or torch.load unless you trust where it came from.",
  error: "The pickle scan could not read this file.",
  pending: "The pickle scan has not finished yet.",
};

function useFileScan(repo: RepoRef, rev: string, path: string, enabled: boolean) {
  return useQuery({
    queryKey: ["file-scan", repo.kind, repo.ns, repo.name, rev, path],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/files/{rev}/{path}/scan", {
          params: { path: { ...repoPathParams(repo), rev, path } },
        }),
      ),
    enabled,
  });
}

function findingName(f: ScanFinding): string {
  if (f.module && f.name) return `${f.module}.${f.name}`;
  return f.module ?? f.name ?? f.detail ?? "";
}

function ScanPanel({
  status,
  repo,
  rev,
  path,
}: {
  status: ScanStatus;
  repo: RepoRef;
  rev: string;
  path: string;
}) {
  const scan = useFileScan(repo, rev, path, status !== "safe");
  const findings = (scan.data?.findings ?? []).filter((f) => f.status !== "safe");
  const tone =
    status === "unsafe"
      ? "border-danger/40 bg-danger-soft"
      : status === "suspicious"
        ? "border-warn/40 bg-warn-soft"
        : "border-line bg-surface";
  return (
    <section
      aria-labelledby="scan-title"
      className={`flex gap-3 rounded-lg border px-4 py-3 ${tone}`}
    >
      <ShieldAlert className="mt-0.5 size-5 shrink-0" aria-hidden="true" />
      <div className="min-w-0 space-y-1 text-sm">
        <h2 id="scan-title" className="flex items-center gap-2 font-semibold">
          Pickle scan <ScanBadge status={status} plain />
        </h2>
        <p>{scanExplanations[status]}</p>
        {findings.length ? (
          <ul aria-label="Imports the scan found" className="space-y-0.5 font-mono text-xs">
            {findings.map((f) => (
              <li key={`${findingName(f)}-${f.opcode ?? ""}`} className="flex flex-wrap gap-x-2">
                <span className="break-all">{findingName(f)}</span>
                <span className="text-muted">{f.status}</span>
              </li>
            ))}
          </ul>
        ) : null}
        {scan.data?.error ? <p className="text-xs text-muted">{scan.data.error}</p> : null}
      </div>
    </section>
  );
}

function Editor({
  repo,
  branch,
  path,
  initial,
  parent,
  onDone,
}: {
  repo: RepoRef;
  branch: string;
  path: string;
  initial: string;
  parent?: string;
  onDone: () => void;
}) {
  const [text, setText] = useState(initial);
  const [summary, setSummary] = useState("");
  const qc = useQueryClient();
  const toast = useToast();
  const creating = parent === undefined;
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/commits", {
          params: { path: repoPathParams(repo) },
          body: {
            summary: summary || `${creating ? "Create" : "Update"} ${path}`,
            branch,
            parent_commit: parent,
            operations: [{ op: "add", path, content_base64: toBase64(text) }],
          },
        }),
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: repoKey(repo) });
      toast(`${creating ? "Created" : "Saved"} ${baseName(path)}.`);
      onDone();
    },
  });
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <Field label={`Edit ${path}`}>
        {(p) => (
          <Textarea
            {...p}
            rows={Math.min(30, Math.max(10, text.split("\n").length + 2))}
            className="font-mono text-[13px]"
            spellCheck={false}
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        )}
      </Field>
      <Field label="Commit message">
        {(p) => (
          <Input
            {...p}
            value={summary}
            placeholder={`${creating ? "Create" : "Update"} ${path}`}
            onChange={(e) => setSummary(e.target.value)}
          />
        )}
      </Field>
      {errorStatus(save.error) === 412 ? (
        <p role="alert" className="text-sm text-danger">
          Someone changed {branch} since you opened this file. Copy your text, reload the page and
          make your change again.
        </p>
      ) : (
        <InlineError error={save.error} />
      )}
      <div className="flex gap-2">
        <Button type="submit" variant="primary" disabled={save.isPending}>
          Commit to {branch}
        </Button>
        <Button onClick={onDone}>Cancel</Button>
      </div>
    </form>
  );
}

function TextContent({
  repo,
  rev,
  path,
  view,
}: {
  repo: RepoRef;
  rev: string;
  path: string;
  view: "markdown" | "json" | "text";
}) {
  const text = useQuery({
    queryKey: [...repoKey(repo), "text", rev, path],
    queryFn: () => fetchText(repo, rev, path),
  });
  const externalImages = useExternalImages();
  const [tab, setTab] = useState(view === "text" ? "source" : "preview");
  if (text.isPending) return <LoadingRows rows={5} label="Loading the file" />;
  if (text.isError) return <ErrorState error={text.error} onRetry={() => text.refetch()} />;
  const content = text.data ?? "";
  const source = <CodeView code={content} lang={languageOf(path)} />;
  if (view === "text") return source;
  let preview: React.ReactNode;
  if (view === "markdown") {
    const { body } = splitFrontMatter(content);
    preview = (
      <div className="rounded-lg border border-line bg-surface p-5">
        <LazyMarkdown
          externalImages={externalImages}
          resolveRelative={(p, k) =>
            k === "image"
              ? resolveUrl(
                  repo.kind,
                  repo.ns,
                  repo.name,
                  rev,
                  [dirName(path), p].filter(Boolean).join("/"),
                )
              : blobPath(
                  repo.kind,
                  repo.ns,
                  repo.name,
                  rev,
                  [dirName(path), p].filter(Boolean).join("/"),
                )
          }
        >
          {body}
        </LazyMarkdown>
      </div>
    );
  } else {
    try {
      preview = (
        <div className="overflow-x-auto rounded-lg border border-line bg-surface p-3 font-mono text-[13px] leading-5">
          <JsonTree value={JSON.parse(content)} />
        </div>
      );
    } catch {
      return source;
    }
  }
  return (
    <Tabs
      label="File view"
      value={tab}
      onValueChange={setTab}
      tabs={[
        { value: "preview", label: view === "markdown" ? "Preview" : "Tree", content: preview },
        { value: "source", label: "Source", content: source },
      ]}
    />
  );
}

/** A file at a Revision: viewers by type, actions, and in-browser editing (BROWSE-4). */
export function BlobPage({
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
  const refs = useRefs(ref);
  const search = useSearch({ strict: false }) as { edit?: string };
  const go = useGo();
  const [editing, setEditing] = useState(search.edit === "1");
  const entry = useQuery({
    queryKey: [...repoKey(ref), "entry", rev, path],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/tree/{rev}/{path}", {
          params: { path: { ...repoPathParams(ref), rev, path } },
        }),
      ),
  });
  const r = repo.data;
  if (!r) return null;
  const isBranch = Boolean(refs.data?.branches.some((b) => b.name === rev));
  const canWrite = Boolean(r.permissions?.write) && !r.mirror && isBranch;
  const view = fileView(path);
  const textual = view === "markdown" || view === "json" || view === "text";
  const header = (
    <div className="flex flex-wrap items-center gap-2">
      <RefPicker repo={ref} rev={rev} build={(v) => blobPath(kind, ns, name, v, path)} />
      <Breadcrumbs kind={kind} ns={ns} name={name} rev={rev} path={path} />
    </div>
  );
  if (entry.isPending)
    return (
      <div className="space-y-3">
        {header}
        <LoadingRows rows={5} label="Loading the file" />
      </div>
    );
  if (entry.isError) {
    if (errorStatus(entry.error) === 404 && editing && canWrite && textual) {
      return (
        <div className="space-y-3">
          {header}
          <Editor
            repo={ref}
            branch={rev}
            path={path}
            initial={path.endsWith("README.md") ? `# ${name}\n\n` : ""}
            onDone={() => go(blobPath(kind, ns, name, rev, path))}
          />
        </div>
      );
    }
    return (
      <div className="space-y-3">
        {header}
        {errorStatus(entry.error) === 404 ? (
          <EmptyState title={`${path} does not exist at ${rev}`}>
            Check the path, or pick another branch or tag.
          </EmptyState>
        ) : (
          <ErrorState error={entry.error} onRetry={() => entry.refetch()} />
        )}
      </div>
    );
  }
  const e = entry.data.items[0];
  if (e?.type !== "file") {
    return (
      <div className="space-y-3">
        {header}
        <EmptyState
          title={`${path} is a folder`}
          action={
            <Button asChild variant="primary">
              <AppLink href={`${repoPath(kind, ns, name)}/tree/${encodeURIComponent(rev)}/${path}`}>
                Open the folder
              </AppLink>
            </Button>
          }
        />
      </div>
    );
  }
  const commit = entry.data.commit;
  const raw = resolveUrl(kind, ns, name, rev, path);
  const permalink = `${window.location.origin}${blobPath(kind, ns, name, commit, path)}`;
  const editable = canWrite && textual && e.size <= maxEdit && !e.lfs;
  const isPickle = pickleExtensions.test(path);
  return (
    <div className="space-y-4">
      {header}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-line bg-surface px-3 py-2">
        <span className="flex flex-wrap items-center gap-1.5">
          <FileCode className="size-4 shrink-0 text-muted" aria-hidden="true" />
          <span className="tabular font-mono text-xs">{formatBytes(e.size)}</span>
          {e.lfs ? <LfsBadge oid={e.lfs.oid} /> : null}
          {!e.present ? <NotPresentBadge /> : null}
          <ScanBadge status={e.scan_status} scope="file" />
        </span>
        {e.last_commit ? (
          <AppLink
            href={`${repoPath(kind, ns, name)}/commit/${e.last_commit.sha}`}
            title={`${e.last_commit.title}, ${formatDateTime(e.last_commit.date)}`}
            className="min-w-0 truncate text-xs text-muted transition-colors duration-100 hover:text-ink hover:underline"
          >
            <span className="font-mono">{shortSha(e.last_commit.sha)}</span> {e.last_commit.title},{" "}
            {relativeTime(e.last_commit.date)}
          </AppLink>
        ) : null}
        <span className="ml-auto flex flex-wrap gap-1">
          <Button asChild size="sm" variant="ghost">
            <a href={raw}>Raw</a>
          </Button>
          {isPickle &&
          (e.scan_status === "unsafe" ||
            e.scan_status === "suspicious" ||
            e.scan_status === "pending") ? (
            <RiskyDownload
              href={resolveUrl(kind, ns, name, rev, path, true)}
              path={path}
              status={e.scan_status}
            />
          ) : (
            <Button asChild size="sm" variant="ghost">
              <a href={resolveUrl(kind, ns, name, rev, path, true)} download={baseName(path)}>
                <Download aria-hidden="true" />
                Download
              </a>
            </Button>
          )}
          <span className="inline-flex items-center">
            <Link2 className="sr-only" aria-hidden="true" />
            <CopyButton value={permalink} label="Copy permalink" showLabel />
          </span>
          <Button asChild size="sm" variant="ghost">
            <AppLink
              href={`${repoPath(kind, ns, name)}/commits/${encodeURIComponent(rev)}?path=${encodeURIComponent(path)}`}
            >
              <History aria-hidden="true" />
              History
            </AppLink>
          </Button>
          {editable && !editing ? (
            <Button size="sm" onClick={() => setEditing(true)}>
              <Pencil aria-hidden="true" />
              Edit
            </Button>
          ) : null}
        </span>
      </div>
      {isPickle && e.scan_status ? (
        <ScanPanel status={e.scan_status} repo={ref} rev={commit} path={path} />
      ) : null}
      {!e.present ? (
        <EmptyState size="sm" title="This file is not in the arsenal">
          This Mirror holds the file's history but not its content yet. Pull it from the Files tab
          to read it here.
        </EmptyState>
      ) : editing && editable ? (
        <EditorLoader
          repo={ref}
          rev={rev}
          path={path}
          parent={commit}
          onDone={() => setEditing(false)}
        />
      ) : view === "safetensors" ? (
        <SafetensorsView repo={ref} rev={rev} path={path} />
      ) : view === "gguf" ? (
        <GgufView repo={ref} rev={rev} path={path} />
      ) : view === "image" ? (
        <div className="rounded-lg border border-line bg-surface p-4">
          <img
            src={raw}
            alt={baseName(path)}
            className="hg-enter mx-auto max-h-[70vh] max-w-full"
          />
        </div>
      ) : textual && e.size <= maxPreview && !e.lfs ? (
        <TextContent repo={ref} rev={rev} path={path} view={view} />
      ) : (
        <EmptyState
          size="sm"
          title={textual ? "This file is too large to show here" : "No preview for this file type"}
        >
          Download it to open it with another tool.
        </EmptyState>
      )}
    </div>
  );
}

function EditorLoader({
  repo,
  rev,
  path,
  parent,
  onDone,
}: {
  repo: RepoRef;
  rev: string;
  path: string;
  parent: string;
  onDone: () => void;
}) {
  const text = useQuery({
    queryKey: [...repoKey(repo), "text", rev, path],
    queryFn: () => fetchText(repo, rev, path),
  });
  if (text.isPending) return <LoadingRows rows={3} label="Loading the file" />;
  if (text.isError) return <ErrorState error={text.error} onRetry={() => text.refetch()} />;
  return (
    <Editor
      repo={repo}
      branch={rev}
      path={path}
      initial={text.data ?? ""}
      parent={parent}
      onDone={onDone}
    />
  );
}
