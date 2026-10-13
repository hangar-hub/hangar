import {
  closestCenter,
  DndContext,
  type DragEndEvent,
  KeyboardSensor,
  PointerSensor,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import {
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { GripVertical, Pencil, Trash2 } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Collection, CollectionItem, Visibility } from "@/api/types";
import { AppLink, useGo } from "@/components/AppLink";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Field, Input, Select, Textarea } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { errorMessage, unwrap } from "@/lib/problem";
import { parseRepoId, repoLabel } from "@/lib/repo";
import { useMe, writableNamespaces } from "@/lib/session";
import { VisibilityBadge } from "../repos/badges";
import { RepoCard } from "../repos/RepoCard";

function ItemView({ item }: { item: CollectionItem }) {
  if (item.repo) return <RepoCard repo={item.repo} layout="list" />;
  const id = parseRepoId(item.item_id, item.type === "dataset" ? "dataset" : "model");
  return (
    <div className="rounded-lg border border-dashed border-line-strong p-3 text-sm">
      {item.type === "collection" ? (
        <AppLink href={`/collections/${item.item_id}`} className="hover:underline">
          Collection {item.item_id}
        </AppLink>
      ) : id ? (
        <span className="font-mono">{repoLabel(id.kind, id.ns, id.name)}</span>
      ) : (
        item.item_id
      )}
      <span className="ml-2 text-xs text-muted">Not available to you</span>
    </div>
  );
}

function SortableItem({
  item,
  editing,
  onRemove,
  onNote,
}: {
  item: CollectionItem;
  editing: boolean;
  onRemove: () => void;
  onNote: (note: string) => void;
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: item.id,
    disabled: !editing,
  });
  const [note, setNote] = useState(item.note ?? "");
  return (
    <li
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={isDragging ? "relative z-10 opacity-80" : undefined}
    >
      <div className="flex items-start gap-2">
        {editing ? (
          <button
            type="button"
            className="mt-3 cursor-grab rounded-sm p-1 text-muted hover:bg-raised active:cursor-grabbing"
            aria-label={`Move ${item.item_id}`}
            {...attributes}
            {...listeners}
          >
            <GripVertical className="size-4" aria-hidden="true" />
          </button>
        ) : null}
        <div className="min-w-0 flex-1 space-y-1">
          <ItemView item={item} />
          {editing ? (
            <form
              className="flex gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                onNote(note);
              }}
            >
              <Input
                aria-label={`Note for ${item.item_id}`}
                placeholder="Add a note"
                value={note}
                onChange={(e) => setNote(e.target.value)}
                className="h-8 text-xs"
              />
              <Button size="sm" type="submit" disabled={note === (item.note ?? "")}>
                Save note
              </Button>
            </form>
          ) : item.note ? (
            <p className="pl-1 text-sm text-muted">{item.note}</p>
          ) : null}
        </div>
        {editing ? (
          <Button
            size="icon-sm"
            variant="ghost"
            className="mt-2"
            aria-label={`Remove ${item.item_id}`}
            onClick={onRemove}
          >
            <Trash2 aria-hidden="true" />
          </Button>
        ) : null}
      </div>
    </li>
  );
}

function AddItem({ c, onAdded }: { c: Collection; onAdded: (c: Collection) => void }) {
  const [id, setId] = useState("");
  const add = useMutation({
    mutationFn: async () => {
      const parsed = parseRepoId(id);
      if (!parsed)
        throw new Error("Enter a Repo ID such as ada/bert-tiny or datasets/ada/imdb-mini.");
      return unwrap(
        await api.POST("/collections/{ns}/{slug}/items", {
          params: { path: { ns: c.namespace, slug: c.slug } },
          body: { type: parsed.kind, item_id: `${parsed.ns}/${parsed.name}` },
        }),
      );
    },
    onSuccess: (x) => {
      setId("");
      onAdded(x);
    },
  });
  return (
    <form
      className="flex flex-wrap items-end gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        add.mutate();
      }}
    >
      <Field label="Add a Repo" hint="Prefix datasets with datasets/." className="min-w-60 flex-1">
        {(p) => (
          <Input
            {...p}
            className="font-mono"
            placeholder="namespace/name"
            value={id}
            onChange={(e) => setId(e.target.value)}
          />
        )}
      </Field>
      <Button type="submit" disabled={!id.trim() || add.isPending}>
        Add
      </Button>
      <div className="w-full">
        <InlineError error={add.error} />
      </div>
    </form>
  );
}

/** A Collection, with an editor for its owners: details, items, notes and drag to reorder (BROWSE-5). */
export function CollectionPage({ ns, slug }: { ns: string; slug: string }) {
  const me = useMe();
  const qc = useQueryClient();
  const toast = useToast();
  const go = useGo();
  const key = ["collections", ns, slug];
  const col = useQuery({
    queryKey: key,
    queryFn: async () =>
      unwrap(await api.GET("/collections/{ns}/{slug}", { params: { path: { ns, slug } } })),
  });
  const [editing, setEditing] = useState(false);
  const [details, setDetails] = useState<{
    title: string;
    description: string;
    visibility: Visibility;
  } | null>(null);
  const sensors = useSensors(
    useSensor(PointerSensor),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  const set = (c: Collection) => qc.setQueryData(key, c);
  const move = useMutation({
    mutationFn: async ({ id, position }: { id: string; position: number }) =>
      unwrap(
        await api.PATCH("/collections/{ns}/{slug}/items/{id}", {
          params: { path: { ns, slug, id } },
          body: { position },
        }),
      ),
    onSuccess: set,
    onError: (e) => {
      toast(errorMessage(e), "danger");
      qc.invalidateQueries({ queryKey: key });
    },
  });
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.PATCH("/collections/{ns}/{slug}", {
          params: { path: { ns, slug } },
          body: details ?? {},
        }),
      ),
    onSuccess: (c) => {
      set(c);
      toast("Collection saved.");
    },
  });
  if (col.isPending) return <LoadingRows rows={4} label="Loading the Collection" />;
  if (col.isError) return <ErrorState error={col.error} onRetry={() => col.refetch()} />;
  const c = col.data;
  const canEdit = writableNamespaces(me.data).includes(c.namespace) || Boolean(me.data?.is_admin);
  const items = [...c.items].sort((a, b) => a.position - b.position);
  const onDragEnd = (e: DragEndEvent) => {
    if (!e.over || e.active.id === e.over.id) return;
    const to = items.findIndex((i) => i.id === e.over?.id);
    const from = items.findIndex((i) => i.id === e.active.id);
    const next = [...items];
    const [moved] = next.splice(from, 1);
    if (!moved) return;
    next.splice(to, 0, moved);
    set({ ...c, items: next.map((i, n) => ({ ...i, position: n })) });
    move.mutate({ id: String(e.active.id), position: to });
  };
  const d = details ?? {
    title: c.title,
    description: c.description ?? "",
    visibility: c.visibility,
  };
  return (
    <PageShell
      title={c.title}
      subtitle={
        <span className="flex flex-wrap items-center gap-2">
          <AppLink href={`/${c.namespace}`} className="font-mono hover:underline">
            {c.namespace}
          </AppLink>
          <VisibilityBadge visibility={c.visibility} />
        </span>
      }
      actions={
        canEdit ? (
          <Button
            variant={editing ? "primary" : "secondary"}
            onClick={() => {
              setEditing(!editing);
              setDetails(null);
            }}
          >
            <Pencil aria-hidden="true" />
            {editing ? "Done" : "Edit"}
          </Button>
        ) : null
      }
    >
      {editing ? (
        <form
          className="grid max-w-2xl gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <Field label="Title">
            {(p) => (
              <Input
                {...p}
                value={d.title}
                onChange={(e) => setDetails({ ...d, title: e.target.value })}
              />
            )}
          </Field>
          <Field label="Description">
            {(p) => (
              <Textarea
                {...p}
                rows={2}
                value={d.description}
                onChange={(e) => setDetails({ ...d, description: e.target.value })}
              />
            )}
          </Field>
          <Field label="Visibility">
            {(p) => (
              <Select
                {...p}
                value={d.visibility}
                onChange={(e) => setDetails({ ...d, visibility: e.target.value as Visibility })}
              >
                <option value="public">Public</option>
                <option value="internal">Internal</option>
                <option value="private">Private</option>
              </Select>
            )}
          </Field>
          <InlineError error={save.error} />
          <div className="flex flex-wrap gap-2">
            <Button type="submit" variant="primary" disabled={!details || save.isPending}>
              Save details
            </Button>
            <ConfirmDialog
              trigger={<Button variant="danger">Delete Collection</Button>}
              title={`Delete ${c.title}?`}
              actionLabel="Delete Collection"
              onConfirm={async () => {
                const res = await api.DELETE("/collections/{ns}/{slug}", {
                  params: { path: { ns, slug } },
                });
                if (!res.response.ok) unwrap(res);
                qc.invalidateQueries({ queryKey: ["collections"] });
                go("/collections");
              }}
            >
              <p>The Repos in it are not affected.</p>
            </ConfirmDialog>
          </div>
        </form>
      ) : c.description ? (
        <p className="max-w-prose">{c.description}</p>
      ) : null}
      {editing ? <AddItem c={c} onAdded={set} /> : null}
      {editing ? (
        <p className="text-xs text-muted">
          Drag items by the handle to reorder them, or focus a handle and use the arrow keys, then
          press Space.
        </p>
      ) : null}
      {items.length === 0 ? (
        <EmptyState title="This Collection is empty">
          {canEdit ? "Choose Edit to add Repos." : "Its owner has not added anything yet."}
        </EmptyState>
      ) : (
        <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
          <SortableContext items={items.map((i) => i.id)} strategy={verticalListSortingStrategy}>
            <ol aria-label="Items" className="space-y-2">
              {items.map((i) => (
                <SortableItem
                  key={i.id}
                  item={i}
                  editing={editing}
                  onRemove={async () => {
                    const res = await api.DELETE("/collections/{ns}/{slug}/items/{id}", {
                      params: { path: { ns, slug, id: i.id } },
                    });
                    if (!res.response.ok)
                      toast(errorMessage(new Error("The item could not be removed.")), "danger");
                    qc.invalidateQueries({ queryKey: key });
                  }}
                  onNote={async (note) => {
                    try {
                      set(
                        unwrap(
                          await api.PATCH("/collections/{ns}/{slug}/items/{id}", {
                            params: { path: { ns, slug, id: i.id } },
                            body: { note },
                          }),
                        ),
                      );
                      toast("Note saved.");
                    } catch (e) {
                      toast(errorMessage(e), "danger");
                    }
                  }}
                />
              ))}
            </ol>
          </SortableContext>
        </DndContext>
      )}
    </PageShell>
  );
}
