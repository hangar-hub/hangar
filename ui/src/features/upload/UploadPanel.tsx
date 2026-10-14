import { useMutation, useQueryClient } from "@tanstack/react-query";
import { FolderUp, Trash2, Upload } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import type { Repo } from "@/api/types";
import { useGo } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/form";
import { ProgressBar } from "@/components/ui/progress";
import { InlineError } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatBytes } from "@/lib/format";
import { apiType, joinPath, repoPath, treePath } from "@/lib/repo";
import { cn } from "@/lib/utils";
import { repoKey, useRefs } from "../repos/hooks";
import { type FileProgress, filesFromDrop, stage, uploadAndCommit } from "./upload";

type Picked = { file: File; relativePath: string };

const stateText: Record<FileProgress["state"], string> = {
  waiting: "Waiting",
  hashing: "Checking",
  uploading: "Uploading",
  stored: "Already stored",
  inline: "Ready",
  done: "Uploaded",
  error: "Failed",
};

/**
 * Upload files or folders into a Repo from the browser (UP-3): drag and
 * drop or pick, then commit to a branch or open a pull request.
 */
export function UploadPanel({
  repo,
  branch,
  folder,
  onClose,
}: {
  repo: Repo;
  branch: string;
  folder: string;
  onClose: () => void;
}) {
  const [picked, setPicked] = useState<Picked[]>([]);
  const [target, setTarget] = useState(folder);
  const [summary, setSummary] = useState("");
  const [description, setDescription] = useState("");
  const [toBranch, setToBranch] = useState(branch);
  const [createPr, setCreatePr] = useState(false);
  const [progress, setProgress] = useState<FileProgress[]>([]);
  const [dragging, setDragging] = useState(false);
  const abort = useRef<AbortController | null>(null);
  const ref = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const refs = useRefs(ref);
  const qc = useQueryClient();
  const toast = useToast();
  const go = useGo();
  const fileInput = useId();
  const folderInput = useId();
  const staged = useMemo(() => stage(picked, target), [picked, target]);
  const total = staged.reduce((a, f) => a + f.file.size, 0);
  const defaultSummary =
    staged.length === 1 ? `Upload ${staged[0]?.path}` : `Upload ${staged.length} files`;

  const run = useMutation({
    mutationFn: async () => {
      abort.current = new AbortController();
      return uploadAndCommit(
        { type: apiType(repo.type), ns: repo.namespace, name: repo.name },
        staged,
        {
          summary: summary.trim() || defaultSummary,
          description: description.trim() || undefined,
          branch: toBranch,
          createPr,
        },
        setProgress,
        abort.current.signal,
      );
    },
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: repoKey(ref) });
      if (res.pull_request) {
        toast(`Opened pull request #${res.pull_request}.`);
        go(`${repoPath(repo.type, repo.namespace, repo.name)}/discussions/${res.pull_request}`);
      } else {
        toast(res.no_op ? "Nothing changed: the files are identical." : "Files committed.");
        go(treePath(repo.type, repo.namespace, repo.name, toBranch, target));
      }
      onClose();
    },
  });

  // Warn before leaving the page while an upload runs (09-web-ui.md).
  useEffect(() => {
    if (!run.isPending) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [run.isPending]);

  const add = (files: Picked[]) =>
    setPicked((prev) => {
      const seen = new Set(prev.map((p) => p.relativePath));
      return [...prev, ...files.filter((f) => !seen.has(f.relativePath))];
    });

  return (
    <section
      aria-labelledby="upload-title"
      className="space-y-4 rounded-lg border border-line bg-surface p-4"
    >
      <div className="flex items-center justify-between gap-2">
        <h2 id="upload-title" className="font-semibold">
          Upload files
        </h2>
        <Button
          size="sm"
          variant="ghost"
          onClick={() => {
            abort.current?.abort();
            onClose();
          }}
        >
          {run.isPending ? "Cancel upload" : "Close"}
        </Button>
      </div>
      {!run.isPending ? (
        // biome-ignore lint/a11y/noStaticElementInteractions: the drop zone adds drag and drop to the labelled inputs inside it
        <div
          onDragOver={(e) => {
            e.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={async (e) => {
            e.preventDefault();
            setDragging(false);
            add(await filesFromDrop(e.dataTransfer.items));
          }}
          className={cn(
            "flex flex-col items-center gap-3 rounded-lg border-2 border-dashed border-line-strong px-4 py-8 text-center",
            dragging && "border-accent-fill bg-accent-soft",
          )}
        >
          <Upload className="size-6 text-muted" aria-hidden="true" />
          <p className="text-sm">Drag files or a folder here, or choose them.</p>
          <div className="flex flex-wrap justify-center gap-2">
            <label
              htmlFor={fileInput}
              className="inline-flex h-9 cursor-pointer items-center gap-1.5 rounded-md border border-line-strong bg-surface px-3.5 text-sm font-medium hover:bg-raised focus-within:outline-2 focus-within:outline-accent"
            >
              Choose files
              <input
                id={fileInput}
                type="file"
                multiple
                className="sr-only"
                onChange={(e) => {
                  add(
                    [...(e.target.files ?? [])].map((file) => ({ file, relativePath: file.name })),
                  );
                  e.target.value = "";
                }}
              />
            </label>
            <label
              htmlFor={folderInput}
              className="inline-flex h-9 cursor-pointer items-center gap-1.5 rounded-md border border-line-strong bg-surface px-3.5 text-sm font-medium hover:bg-raised focus-within:outline-2 focus-within:outline-accent"
            >
              <FolderUp className="size-4" aria-hidden="true" />
              Choose a folder
              <input
                id={folderInput}
                type="file"
                multiple
                className="sr-only"
                {...{ webkitdirectory: "" }}
                onChange={(e) => {
                  add(
                    [...(e.target.files ?? [])].map((file) => ({
                      file,
                      relativePath: file.webkitRelativePath || file.name,
                    })),
                  );
                  e.target.value = "";
                }}
              />
            </label>
          </div>
        </div>
      ) : null}
      {staged.length ? (
        <>
          <Field label="Upload into folder" hint="Leave empty for the Repo root.">
            {(p) => (
              <Input
                {...p}
                className="font-mono"
                value={target}
                disabled={run.isPending}
                onChange={(e) => setTarget(e.target.value)}
                placeholder="Repo root"
              />
            )}
          </Field>
          <ul
            aria-label="Files to upload"
            className="divide-y divide-line rounded-md border border-line"
          >
            {staged.map((f, i) => {
              const p = progress[i];
              return (
                <li key={f.path} className="space-y-1 px-3 py-2">
                  <div className="flex items-center justify-between gap-3 text-sm">
                    <span className="min-w-0 break-all font-mono text-xs">{joinPath(f.path)}</span>
                    <span className="flex shrink-0 items-center gap-2">
                      <span className="tabular font-mono text-xs text-muted">
                        {formatBytes(f.file.size)}
                      </span>
                      {p ? <span className="text-xs">{stateText[p.state]}</span> : null}
                      {!run.isPending ? (
                        <Button
                          size="icon-sm"
                          variant="ghost"
                          aria-label={`Remove ${f.path}`}
                          onClick={() => setPicked(picked.filter((_, j) => j !== i))}
                        >
                          <Trash2 aria-hidden="true" />
                        </Button>
                      ) : null}
                    </span>
                  </div>
                  {p && (p.state === "hashing" || p.state === "uploading") ? (
                    <ProgressBar
                      done={p.state === "hashing" ? p.hashed : p.uploaded}
                      total={p.size}
                      label={`${p.state === "hashing" ? "Checking" : "Uploading"} ${f.path}`}
                    />
                  ) : null}
                </li>
              );
            })}
          </ul>
          <p className="tabular text-xs text-muted">
            {staged.length} {staged.length === 1 ? "file" : "files"}, {formatBytes(total)}
          </p>
          <form
            className="space-y-3"
            onSubmit={(e) => {
              e.preventDefault();
              run.mutate();
            }}
          >
            <Field label="Commit message">
              {(p) => (
                <Input
                  {...p}
                  value={summary}
                  placeholder={defaultSummary}
                  disabled={run.isPending}
                  onChange={(e) => setSummary(e.target.value)}
                />
              )}
            </Field>
            <Field label="Description" hint="Optional.">
              {(p) => (
                <Textarea
                  {...p}
                  rows={2}
                  value={description}
                  disabled={run.isPending}
                  onChange={(e) => setDescription(e.target.value)}
                />
              )}
            </Field>
            <div className="flex flex-wrap items-end gap-4">
              <Field label="Branch">
                {(p) => (
                  <Select
                    {...p}
                    className="w-auto font-mono"
                    value={toBranch}
                    disabled={run.isPending || createPr}
                    onChange={(e) => setToBranch(e.target.value)}
                  >
                    {(refs.data?.branches ?? [{ name: branch, ref: "", target_commit: "" }]).map(
                      (b) => (
                        <option key={b.name} value={b.name}>
                          {b.name}
                        </option>
                      ),
                    )}
                  </Select>
                )}
              </Field>
              <Checkbox
                label="Open as pull request"
                checked={createPr}
                disabled={run.isPending}
                onChange={(e) => setCreatePr(e.target.checked)}
              />
            </div>
            <InlineError error={run.error} />
            <Button type="submit" variant="primary" disabled={run.isPending || staged.length === 0}>
              {run.isPending
                ? "Uploading"
                : createPr
                  ? "Upload and open pull request"
                  : "Upload and commit"}
            </Button>
          </form>
        </>
      ) : null}
    </section>
  );
}
