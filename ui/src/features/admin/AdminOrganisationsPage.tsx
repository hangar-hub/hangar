import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api/client";
import { AppLink } from "@/components/AppLink";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { fieldErrors, unwrap } from "@/lib/problem";
import { useSearchState } from "@/lib/use-search-state";

/**
 * Organisations: every Organisation of the Instance, filtered by name, and
 * a form to create new ones.
 */
export function AdminOrganisationsPage() {
  const [search, update] = useSearchState<{ q?: string }>();
  const qc = useQueryClient();
  const toast = useToast();
  const results = useInfiniteQuery({
    queryKey: ["admin", "orgs", search.q ?? ""],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/admin/orgs", {
          params: { query: { q: search.q || undefined, cursor: pageParam, limit: 100 } },
        }),
      ),
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  const [form, setForm] = useState({ name: "", display_name: "", description: "" });
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/orgs", {
          body: {
            name: form.name,
            display_name: form.display_name || undefined,
            description: form.description || undefined,
          },
        }),
      ),
    onSuccess: (n) => {
      toast(`Created ${n.name}.`);
      setForm({ name: "", display_name: "", description: "" });
      qc.invalidateQueries({ queryKey: ["admin", "orgs"] });
    },
  });
  const errs = fieldErrors(create.error);
  const orgs = results.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <PageShell title="Organisations" subtitle="Manage Members from each Organisation's page.">
      <form
        className="max-w-sm"
        onSubmit={(e) => {
          e.preventDefault();
          const q = new FormData(e.currentTarget).get("q");
          update({ q: typeof q === "string" && q ? q : undefined });
        }}
      >
        <Input
          name="q"
          type="search"
          aria-label="Find an Organisation"
          placeholder="Find an Organisation by name"
          key={search.q ?? ""}
          defaultValue={search.q ?? ""}
        />
      </form>
      {results.isPending ? (
        <LoadingRows rows={2} label="Loading Organisations" />
      ) : results.isError ? (
        <ErrorState error={results.error} onRetry={() => results.refetch()} />
      ) : orgs.length === 0 ? (
        <EmptyState
          title={search.q ? `No Organisations match ${search.q}` : "No Organisations yet"}
        />
      ) : (
        <ul className="hg-enter divide-y divide-line rounded-lg border border-line bg-surface">
          {orgs.map((o) => (
            <li
              key={o.name}
              className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-sm"
            >
              <AppLink
                href={`/${o.name}?tab=members`}
                className="font-medium underline-offset-2 wrap-anywhere hover:underline"
              >
                {o.display_name || o.name}
              </AppLink>
              <span className="font-mono text-xs text-muted">{o.name}</span>
              {o.reserved_mirror ? <Badge tone="info">Created by a Pull</Badge> : null}
              {o.member_count !== undefined ? (
                <span className="tabular ml-auto text-xs text-muted">{o.member_count} members</span>
              ) : null}
            </li>
          ))}
        </ul>
      )}
      {results.hasNextPage ? (
        <Button onClick={() => results.fetchNextPage()} disabled={results.isFetchingNextPage}>
          Show more
        </Button>
      ) : null}
      <form
        className="max-w-md space-y-3 pt-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <h2 className="font-semibold">Create an Organisation</h2>
        <Field label="Name" error={errs.name}>
          {(p) => (
            <Input
              {...p}
              required
              className="font-mono"
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
          )}
        </Field>
        <Field label="Display name">
          {(p) => (
            <Input
              {...p}
              value={form.display_name}
              onChange={(e) => setForm({ ...form, display_name: e.target.value })}
            />
          )}
        </Field>
        <Field label="Description">
          {(p) => (
            <Input
              {...p}
              value={form.description}
              onChange={(e) => setForm({ ...form, description: e.target.value })}
            />
          )}
        </Field>
        {create.error && !errs.name ? <InlineError error={create.error} /> : null}
        <Button type="submit" variant="primary" disabled={!form.name || create.isPending}>
          Create Organisation
        </Button>
      </form>
    </PageShell>
  );
}
