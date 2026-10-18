import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { GitPullRequest, MessageSquare, Plus } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Discussion } from "@/api/types";
import { AppLink, useGo } from "@/components/AppLink";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Textarea } from "@/components/ui/form";
import { SegmentedControl } from "@/components/ui/segmented";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { formatDateTime, relativeTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { type RepoKind, repoPath } from "@/lib/repo";
import { useSearchState } from "@/lib/use-search-state";
import { type RepoRef, repoKey, repoPathParams, useRepo } from "../repos/hooks";

export const statusTone = {
  open: "ok",
  draft: "neutral",
  closed: "danger",
  merged: "accent",
} as const;
export const statusText = {
  open: "Open",
  draft: "Draft",
  closed: "Closed",
  merged: "Merged",
} as const;

function NewDiscussion({ repo }: { repo: RepoRef }) {
  const [open, setOpen] = useState(false);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const qc = useQueryClient();
  const go = useGo();
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/discussions", {
          params: { path: repoPathParams(repo) },
          body: { title, description },
        }),
      ),
    onSuccess: (d) => {
      qc.invalidateQueries({ queryKey: [...repoKey(repo), "discussions"] });
      setOpen(false);
      go(`${repoPath(repo.kind, repo.ns, repo.name)}/discussions/${d.num}`);
    },
  });
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      wide
      title="New discussion"
      trigger={
        <Button variant="primary" size="sm">
          <Plus aria-hidden="true" />
          New discussion
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="new-discussion"
            disabled={!title.trim() || create.isPending}
          >
            Start discussion
          </Button>
        </>
      }
    >
      <form
        id="new-discussion"
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Title">
          {(p) => (
            <Input {...p} required value={title} onChange={(e) => setTitle(e.target.value)} />
          )}
        </Field>
        <Field label="Description" hint="Markdown is supported.">
          {(p) => (
            <Textarea
              {...p}
              rows={8}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          )}
        </Field>
        <InlineError error={create.error} />
        <p className="text-xs text-muted">
          To propose file changes, upload them from the Files tab with{" "}
          <strong>Open as pull request</strong>, or push to a new pull request with{" "}
          <code className="font-mono">create_pr=True</code>.
        </p>
      </form>
    </Dialog>
  );
}

function Item({ d, base }: { d: Discussion; base: string }) {
  const Icon = d.kind === "pull_request" ? GitPullRequest : MessageSquare;
  return (
    <li className="flex items-start gap-3 px-4 py-3">
      <Icon
        className="mt-0.5 size-4 shrink-0 text-muted"
        aria-label={d.kind === "pull_request" ? "Pull request" : "Discussion"}
      />
      <div className="min-w-0 flex-1">
        <AppLink
          href={`${base}/discussions/${d.num}`}
          className="font-medium underline-offset-2 wrap-anywhere hover:underline"
        >
          {d.title}
        </AppLink>
        <p className="mt-0.5 text-xs text-muted">
          <span className="tabular">#{d.num}</span> opened{" "}
          <time dateTime={d.created_at} title={formatDateTime(d.created_at)} className="tabular">
            {relativeTime(d.created_at)}
          </time>{" "}
          by {d.author}
        </p>
      </div>
      <Badge tone={statusTone[d.status]} className="shrink-0">
        {statusText[d.status]}
      </Badge>
    </li>
  );
}

/** The Community tab: discussions and pull requests (REPO-6). */
export function DiscussionsPage({ kind, ns, name }: { kind: RepoKind; ns: string; name: string }) {
  const ref: RepoRef = { kind, ns, name };
  const repo = useRepo(ref);
  const [search, update] = useSearchState<{ kind?: string; status?: string }>();
  const filterKind =
    search.kind === "discussion" || search.kind === "pull_request" ? search.kind : undefined;
  const status = search.status === "closed" ? "closed" : "open";
  const list = useInfiniteQuery({
    queryKey: [...repoKey(ref), "discussions", filterKind ?? "", status],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/discussions", {
          params: {
            path: repoPathParams(ref),
            query: {
              status,
              ...(filterKind ? { kind: filterKind } : {}),
              ...(pageParam ? { cursor: pageParam } : {}),
            },
          },
        }),
      ),
    getNextPageParam: (l) => l.next_cursor || undefined,
  });
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  const base = repoPath(kind, ns, name);
  const canDiscuss = Boolean(repo.data?.permissions?.discuss);
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <SegmentedControl<typeof filterKind>
          legend="Type"
          value={filterKind}
          onValueChange={(v) => update({ kind: v })}
          options={[
            { value: undefined, label: "All" },
            { value: "discussion", label: "Discussions" },
            { value: "pull_request", label: "Pull requests" },
          ]}
        />
        <SegmentedControl
          legend="Status"
          value={status}
          onValueChange={(v) => update({ status: v === "open" ? undefined : v })}
          options={[
            { value: "open", label: "Open" },
            { value: "closed", label: "Closed" },
          ]}
        />
        <div className="ml-auto">{canDiscuss ? <NewDiscussion repo={ref} /> : null}</div>
      </div>
      {list.isPending ? (
        <LoadingRows rows={4} label="Loading discussions" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState
          className="hg-enter"
          title={status === "open" ? "No open discussions" : "No closed discussions"}
        >
          Discussions and pull requests about this Repo appear here.
        </EmptyState>
      ) : (
        <ul
          aria-label="Discussions"
          className="hg-enter divide-y divide-line rounded-lg border border-line bg-surface"
        >
          {items.map((d) => (
            <Item key={d.num} d={d} base={base} />
          ))}
        </ul>
      )}
      {list.hasNextPage ? <Button onClick={() => list.fetchNextPage()}>Show more</Button> : null}
    </div>
  );
}
