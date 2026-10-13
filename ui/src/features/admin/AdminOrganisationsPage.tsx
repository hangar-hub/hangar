import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
 * Organisations: find one by name and create new ones. The native API has
 * no list of every Organisation, so this page searches Namespaces
 * (docs/build/requests/w8.md asks for `GET /orgs`).
 */
export function AdminOrganisationsPage() {
  const [search, update] = useSearchState<{ q?: string }>();
  const qc = useQueryClient();
  const toast = useToast();
  const results = useQuery({
    queryKey: ["search", "orgs", search.q ?? ""],
    enabled: Boolean(search.q),
    queryFn: async () =>
      unwrap(
        await api.GET("/search", {
          params: { query: { q: search.q ?? "", types: ["namespaces"], limit: 50 } },
        }),
      ),
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
      qc.invalidateQueries({ queryKey: ["search"] });
    },
  });
  const errs = fieldErrors(create.error);
  const orgs = results.data?.namespaces.filter((n) => n.kind === "org") ?? [];
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
      {!search.q ? null : results.isPending ? (
        <LoadingRows rows={2} label="Searching" />
      ) : results.isError ? (
        <ErrorState error={results.error} onRetry={() => results.refetch()} />
      ) : orgs.length === 0 ? (
        <EmptyState title={`No Organisations match ${search.q}`} />
      ) : (
        <ul className="divide-y divide-line rounded-lg border border-line bg-surface">
          {orgs.map((o) => (
            <li key={o.name} className="flex flex-wrap items-center gap-2 px-4 py-2 text-sm">
              <AppLink href={`/${o.name}?tab=members`} className="font-medium hover:underline">
                {o.display_name || o.name}
              </AppLink>
              <span className="font-mono text-xs text-muted">{o.name}</span>
              {o.reserved_mirror ? <Badge tone="info">Created by a Pull</Badge> : null}
              {o.member_count !== undefined ? (
                <span className="ml-auto text-xs text-muted">{o.member_count} members</span>
              ) : null}
            </li>
          ))}
        </ul>
      )}
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
