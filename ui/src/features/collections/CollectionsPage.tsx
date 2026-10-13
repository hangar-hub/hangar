import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Library, Plus } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Visibility } from "@/api/types";
import { AppLink, useGo } from "@/components/AppLink";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Select, Textarea } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { relativeTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { useMe, writableNamespaces } from "@/lib/session";
import { useSearchState } from "@/lib/use-search-state";
import { VisibilityBadge } from "../repos/badges";

function CreateCollection() {
  const me = useMe();
  const go = useGo();
  const qc = useQueryClient();
  const owners = writableNamespaces(me.data);
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    namespace: "",
    title: "",
    description: "",
    visibility: "public" as Visibility,
  });
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/collections", {
          body: { ...form, namespace: form.namespace || owners[0] || "" },
        }),
      ),
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ["collections"] });
      setOpen(false);
      go(`/collections/${c.namespace}/${c.slug}`);
    },
  });
  if (!me.data) return null;
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title="New Collection"
      description="A named, ordered list of Repos to share."
      trigger={
        <Button variant="primary">
          <Plus aria-hidden="true" />
          New Collection
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="new-collection"
            disabled={!form.title.trim() || create.isPending}
          >
            Create Collection
          </Button>
        </>
      }
    >
      <form
        id="new-collection"
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Owner">
          {(p) => (
            <Select
              {...p}
              value={form.namespace || owners[0]}
              onChange={(e) => setForm({ ...form, namespace: e.target.value })}
            >
              {owners.map((o) => (
                <option key={o} value={o}>
                  {o}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Title">
          {(p) => (
            <Input
              {...p}
              required
              value={form.title}
              onChange={(e) => setForm({ ...form, title: e.target.value })}
            />
          )}
        </Field>
        <Field label="Description">
          {(p) => (
            <Textarea
              {...p}
              rows={2}
              value={form.description}
              onChange={(e) => setForm({ ...form, description: e.target.value })}
            />
          )}
        </Field>
        <Field label="Visibility">
          {(p) => (
            <Select
              {...p}
              value={form.visibility}
              onChange={(e) => setForm({ ...form, visibility: e.target.value as Visibility })}
            >
              <option value="public">Public</option>
              <option value="internal">Internal</option>
              <option value="private">Private</option>
            </Select>
          )}
        </Field>
        <InlineError error={create.error} />
      </form>
    </Dialog>
  );
}

/** Collections the user can see (BROWSE-5). */
export function CollectionsPage() {
  const [search, update] = useSearchState<{ q?: string }>();
  const list = useQuery({
    queryKey: ["collections", "all", search.q ?? ""],
    queryFn: async () =>
      unwrap(
        await api.GET("/collections", {
          params: { query: { ...(search.q ? { q: search.q } : {}), limit: 200 } },
        }),
      ),
  });
  return (
    <PageShell
      title="Collections"
      subtitle="Curated, ordered lists of models and datasets."
      actions={<CreateCollection />}
    >
      <form
        className="max-w-md"
        onSubmit={(e) => {
          e.preventDefault();
          const q = new FormData(e.currentTarget).get("q");
          update({ q: typeof q === "string" && q ? q : undefined });
        }}
      >
        <Input
          name="q"
          type="search"
          aria-label="Search Collections"
          placeholder="Search Collections"
          key={search.q ?? ""}
          defaultValue={search.q ?? ""}
        />
      </form>
      {list.isPending ? (
        <LoadingRows rows={3} label="Loading Collections" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : list.data.items.length === 0 ? (
        <EmptyState
          icon={<Library />}
          title={search.q ? "No Collections match" : "No Collections yet"}
        >
          Group the models and datasets you use together, such as the ones that run on a laptop.
        </EmptyState>
      ) : (
        <ul className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
          {list.data.items.map((c) => (
            <li
              key={`${c.namespace}/${c.slug}`}
              className="relative rounded-lg border border-line bg-surface p-4 hover:border-line-strong"
            >
              <h2 className="font-medium">
                <AppLink
                  href={`/collections/${c.namespace}/${c.slug}`}
                  className="after:absolute after:inset-0 hover:underline"
                >
                  {c.title}
                </AppLink>
              </h2>
              {c.description ? (
                <p className="mt-1 line-clamp-2 text-sm text-muted">{c.description}</p>
              ) : null}
              <p className="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted">
                <span className="font-mono">{c.namespace}</span>
                <span>
                  {c.items.length} {c.items.length === 1 ? "item" : "items"}
                </span>
                <span>Updated {relativeTime(c.updated_at)}</span>
                {c.visibility !== "public" ? <VisibilityBadge visibility={c.visibility} /> : null}
              </p>
            </li>
          ))}
        </ul>
      )}
    </PageShell>
  );
}
