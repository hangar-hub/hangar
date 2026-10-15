import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api/client";
import type { Discussion, DiscussionEvent } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { LazyMarkdown } from "@/components/content/LazyMarkdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Field, Textarea } from "@/components/ui/form";
import { ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { Tabs } from "@/components/ui/tabs";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, relativeTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { type RepoKind, repoPath, treePath } from "@/lib/repo";
import { useMe } from "@/lib/session";
import { ChangeList } from "../files/ChangeList";
import { type RepoRef, repoKey, repoPathParams, useRefs, useRepo } from "../repos/hooks";
import { useExternalImages } from "../repos/useInstance";
import { statusText, statusTone } from "./DiscussionsPage";

function Event({ e }: { e: DiscussionEvent }) {
  const external = useExternalImages();
  if (e.type === "comment") {
    return (
      <li className="rounded-lg border border-line bg-surface">
        <p className="border-b border-line px-4 py-2 text-xs text-muted">
          <span className="font-medium text-ink">{e.author}</span> commented{" "}
          <time dateTime={e.created_at} title={formatDateTime(e.created_at)}>
            {relativeTime(e.created_at)}
          </time>
        </p>
        <div className="px-4 py-3">
          {e.hidden ? (
            <p className="text-sm text-muted">This comment is hidden.</p>
          ) : (
            <LazyMarkdown externalImages={external}>{e.body ?? ""}</LazyMarkdown>
          )}
        </div>
      </li>
    );
  }
  const data = (e.data ?? {}) as Record<string, string>;
  const text =
    e.type === "status-change"
      ? `changed the status to ${statusText[(data.status as keyof typeof statusText) ?? "open"]?.toLowerCase() ?? data.status}`
      : e.type === "title-change"
        ? `renamed this from "${data.from}" to "${data.to}"`
        : "added a Commit";
  return (
    <li className="px-4 text-sm text-muted">
      <span className="font-medium text-ink">{e.author}</span> {text} {relativeTime(e.created_at)}
      {e.body ? <span>: {e.body}</span> : null}
    </li>
  );
}

/** One discussion or pull request: thread, comments, status changes and merge (REPO-6). */
export function DiscussionPage({
  kind,
  ns,
  name,
  num,
}: {
  kind: RepoKind;
  ns: string;
  name: string;
  num: number;
}) {
  const ref: RepoRef = { kind, ns, name };
  const repo = useRepo(ref);
  const refs = useRefs(ref);
  const me = useMe();
  const qc = useQueryClient();
  const toast = useToast();
  const key = [...repoKey(ref), "discussion", num];
  const [comment, setComment] = useState("");
  const [tab, setTab] = useState("thread");
  const d = useQuery({
    queryKey: key,
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/discussions/{num}", {
          params: { path: { ...repoPathParams(ref), num } },
        }),
      ),
  });
  const set = (x: Discussion) => {
    qc.setQueryData(key, x);
    qc.invalidateQueries({ queryKey: [...repoKey(ref), "discussions"] });
  };
  const post = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/discussions/{num}/comments", {
          params: { path: { ...repoPathParams(ref), num } },
          body: { body: comment },
        }),
      ),
    onSuccess: () => {
      setComment("");
      qc.invalidateQueries({ queryKey: key });
    },
  });
  const setStatus = useMutation({
    mutationFn: async (status: "open" | "closed") =>
      unwrap(
        await api.PATCH("/repos/{type}/{ns}/{name}/discussions/{num}", {
          params: { path: { ...repoPathParams(ref), num } },
          body: { status, ...(comment ? { comment } : {}) },
        }),
      ),
    onSuccess: (x) => {
      setComment("");
      set(x);
    },
  });
  if (d.isPending) return <LoadingRows rows={4} label="Loading the discussion" />;
  if (d.isError) return <ErrorState error={d.error} onRetry={() => d.refetch()} />;
  const x = d.data;
  const r = repo.data;
  const isPr = x.kind === "pull_request";
  const canWrite = Boolean(r?.permissions?.write);
  const canClose = canWrite || me.data?.username === x.author;
  const prHead = refs.data?.pull_requests.find((p) => p.ref === x.pr_ref)?.target_commit;
  const thread = (
    <div className="space-y-4">
      <ol aria-label="Thread" className="space-y-3">
        {(x.events ?? []).map((e) => (
          <Event key={e.id} e={e} />
        ))}
      </ol>
      {r?.permissions?.discuss ? (
        <form
          className="space-y-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (comment.trim()) post.mutate();
          }}
        >
          <Field label="Add a comment" hint="Markdown is supported.">
            {(p) => (
              <Textarea
                {...p}
                rows={5}
                value={comment}
                onChange={(e) => setComment(e.target.value)}
              />
            )}
          </Field>
          <InlineError error={post.error ?? setStatus.error} />
          <div className="flex flex-wrap gap-2">
            <Button type="submit" variant="primary" disabled={!comment.trim() || post.isPending}>
              Comment
            </Button>
            {canClose && x.status !== "merged" ? (
              <Button
                onClick={() => setStatus.mutate(x.status === "closed" ? "open" : "closed")}
                disabled={setStatus.isPending}
              >
                {x.status === "closed" ? "Reopen" : comment.trim() ? "Close with comment" : "Close"}
              </Button>
            ) : null}
          </div>
        </form>
      ) : null}
    </div>
  );
  return (
    <div className="space-y-4">
      <header className="space-y-2">
        <AppLink
          href={`${repoPath(kind, ns, name)}/discussions`}
          className="text-sm text-accent hover:underline"
        >
          All discussions
        </AppLink>
        <h2 className="text-xl font-semibold">
          {x.title} <span className="font-normal text-muted">#{x.num}</span>
        </h2>
        <div className="flex flex-wrap items-center gap-2 text-sm text-muted">
          <Badge tone={statusTone[x.status]}>{statusText[x.status]}</Badge>
          <Badge>{isPr ? "Pull request" : "Discussion"}</Badge>
          <span>
            {x.author} opened this {relativeTime(x.created_at)}
          </span>
          {isPr && x.pr_ref ? (
            <span className="font-mono text-xs">
              {x.pr_ref} into {x.pr_base ?? r?.default_branch}
            </span>
          ) : null}
          {isPr && x.status === "open" && canWrite ? (
            <ConfirmDialog
              danger={false}
              trigger={
                <Button size="sm" variant="primary" className="ml-auto">
                  Merge pull request
                </Button>
              }
              title="Merge this pull request?"
              actionLabel="Merge"
              onConfirm={async () => {
                set(
                  unwrap(
                    await api.POST("/repos/{type}/{ns}/{name}/discussions/{num}/merge", {
                      params: { path: { ...repoPathParams(ref), num } },
                      body: {},
                    }),
                  ),
                );
                qc.invalidateQueries({ queryKey: repoKey(ref) });
                toast(`Merged #${x.num} into ${x.pr_base ?? r?.default_branch}.`);
              }}
            >
              <p>
                The changes in <code className="font-mono">{x.pr_ref}</code> become a Commit on{" "}
                <code className="font-mono">{x.pr_base ?? r?.default_branch}</code>.
              </p>
            </ConfirmDialog>
          ) : null}
        </div>
      </header>
      {isPr ? (
        <Tabs
          label="Pull request"
          value={tab}
          onValueChange={setTab}
          tabs={[
            { value: "thread", label: "Conversation", content: thread },
            {
              value: "files",
              label: `Files changed (${x.changes?.length ?? 0})`,
              content: (
                <div className="space-y-2">
                  {prHead ? (
                    <Button asChild size="sm">
                      <AppLink href={treePath(kind, ns, name, x.pr_ref ?? prHead)}>
                        Browse files on {x.pr_ref}
                      </AppLink>
                    </Button>
                  ) : null}
                  {x.changes?.length ? (
                    <ChangeList
                      repo={ref}
                      sha={prHead ?? ""}
                      pullRequest={x.num}
                      changes={x.changes}
                    />
                  ) : (
                    <p className="text-sm text-muted">No files changed yet.</p>
                  )}
                </div>
              ),
            },
          ]}
        />
      ) : (
        thread
      )}
    </div>
  );
}
