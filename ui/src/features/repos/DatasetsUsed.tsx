import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Database, Plus, X } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { api } from "@/api/client";
import type { Repo } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { InlineError, LoadingRows } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { cardDatasets, isDatasetId, withCardDatasets } from "@/lib/card-datasets";
import { parseCardData, splitFrontMatter } from "@/lib/frontmatter";
import { errorMessage, unwrap } from "@/lib/problem";
import { repoPath } from "@/lib/repo";
import { type RepoRef, repoKey, repoPathParams, useTextFile } from "./hooks";

function toBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

/**
 * The **Datasets used** picker of a model: chooses datasets from the
 * arsenal (or names one on an Upstream) and commits them to the Model
 * card's `datasets:` metadata, as the Hub stores them. Each dataset then
 * lists the model under **Used by**. Links are optional.
 */
export function DatasetsUsed({ repo }: { repo: Repo }) {
  const ref: RepoRef = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const readme = useTextFile(ref, repo.default_branch, "README.md");
  const qc = useQueryClient();
  const toast = useToast();
  const [chosen, setChosen] = useState<string[] | null>(null);
  const [find, setFind] = useState("");
  const query = useDebounced(find.trim(), 200);

  const parsed = useMemo(() => {
    if (readme.data === undefined) return undefined;
    try {
      return { saved: cardDatasets(parseCardData(splitFrontMatter(readme.data ?? "").yaml)) };
    } catch (e) {
      return { saved: [] as string[], error: errorMessage(e) };
    }
  }, [readme.data]);
  const saved = parsed?.saved ?? [];
  const list = chosen ?? saved;
  const changed = chosen !== null && chosen.join("\n") !== saved.join("\n");

  const matches = useQuery({
    queryKey: ["repos", { type: "datasets", q: query, limit: 8 }],
    enabled: query.length > 0,
    placeholderData: keepPreviousData,
    queryFn: async () =>
      unwrap(
        await api.GET("/repos", { params: { query: { type: "datasets", q: query, limit: 8 } } }),
      ),
  });
  const found = query ? (matches.data?.items ?? []).filter((d) => !list.includes(d.id)) : [];
  const typed = find.trim();
  const canAddTyped =
    isDatasetId(typed) && !list.includes(typed) && !found.some((d) => d.id === typed);

  const add = (id: string) => {
    setChosen([...list, id]);
    setFind("");
  };
  const remove = (id: string) => setChosen(list.filter((x) => x !== id));

  const save = useMutation({
    mutationFn: async () => {
      const content = withCardDatasets(readme.data ?? null, list);
      return unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/commits", {
          params: { path: repoPathParams(ref) },
          body: {
            summary: "Update datasets used",
            parent_commit: repo.sha,
            branch: repo.default_branch,
            operations: [{ op: "add", path: "README.md", content_base64: toBase64(content) }],
          },
        }),
      );
    },
    onSuccess: async () => {
      toast("Datasets used saved.");
      // Reload the card before showing it as saved, so the list does not
      // flick back to the old metadata.
      await qc.invalidateQueries({ queryKey: repoKey(ref) });
      setChosen(null);
      // Each dataset's Used by list comes from the model's Dependencies at
      // its new head. Reading them makes Hangar resolve them now rather
      // than when the index Job runs, so the datasets list the model at
      // once. If the read fails, the index Job still resolves them.
      try {
        await qc.fetchQuery({
          queryKey: [...repoKey(ref), "dependencies"],
          queryFn: async () =>
            unwrap(
              await api.GET("/repos/{type}/{ns}/{name}/dependencies", {
                params: { path: repoPathParams(ref) },
              }),
            ),
        });
      } catch {
        // The index Job resolves them instead; nothing to tell the user.
      }
      await qc.invalidateQueries({ queryKey: ["repo", "datasets"] });
    },
  });

  if (readme.isPending) return <LoadingRows rows={2} label="Loading the Model card" />;
  if (readme.isError) return <InlineError error={readme.error} />;
  if (parsed?.error) {
    return (
      <p role="alert" className="text-sm text-warn">
        The Model card's YAML metadata is not valid, so Hangar cannot change it here: {parsed.error}{" "}
        Fix it in <code className="font-mono">README.md</code> first.
      </p>
    );
  }
  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (changed) save.mutate();
      }}
    >
      {list.length ? (
        <ul
          aria-label="Datasets used"
          className="divide-y divide-line rounded-md border border-line"
        >
          {list.map((id) => (
            <li key={id} className="flex items-center justify-between gap-2 px-3 py-1.5">
              <span className="flex min-w-0 items-center gap-2">
                <Database className="size-4 shrink-0 text-muted" aria-hidden="true" />
                {id.includes("/") ? (
                  <AppLink
                    href={repoPath("dataset", id.split("/")[0] ?? "", id.split("/")[1] ?? "")}
                    className="break-all font-mono text-sm hover:underline"
                  >
                    {id}
                  </AppLink>
                ) : (
                  <span className="break-all font-mono text-sm">{id}</span>
                )}
              </span>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Remove ${id}`}
                onClick={() => remove(id)}
              >
                <X aria-hidden="true" />
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted">
          No datasets listed. That is fine: links between models and datasets are optional.
        </p>
      )}
      <Field
        label="Find a dataset"
        hint="Search the Arsenal by name, or type the ID of a dataset on an Upstream, such as imdb."
      >
        {(p) => (
          <Input
            {...p}
            type="search"
            value={find}
            autoComplete="off"
            onChange={(e) => setFind(e.target.value)}
          />
        )}
      </Field>
      {found.length || canAddTyped ? (
        <ul aria-label="Datasets to add" className="space-y-1">
          {found.map((d) => (
            <li key={d.id}>
              <Button size="sm" variant="ghost" onClick={() => add(d.id)}>
                <Plus aria-hidden="true" />
                Add <span className="font-mono">datasets/{d.id}</span>
              </Button>
            </li>
          ))}
          {canAddTyped ? (
            <li>
              <Button size="sm" variant="ghost" onClick={() => add(typed)}>
                <Plus aria-hidden="true" />
                Add <span className="font-mono">{typed}</span> by ID
              </Button>
            </li>
          ) : null}
        </ul>
      ) : null}
      <p role="status" className="text-sm text-muted empty:hidden">
        {query && !matches.isFetching && !found.length && !canAddTyped
          ? `No datasets in the Arsenal match ${query}.`
          : ""}
      </p>
      <InlineError error={save.error} />
      <div className="flex gap-2">
        <Button type="submit" variant="primary" disabled={!changed || save.isPending}>
          Commit to the Model card
        </Button>
        {changed ? <Button onClick={() => setChosen(null)}>Undo changes</Button> : null}
      </div>
    </form>
  );
}
