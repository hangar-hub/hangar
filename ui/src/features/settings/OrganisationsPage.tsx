import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api/client";
import { AppLink, useGo } from "@/components/AppLink";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { EmptyState, InlineError } from "@/components/ui/states";
import { fieldErrors, unwrap } from "@/lib/problem";
import { meKey, useMe } from "@/lib/session";

/** Organisations you belong to, and a form to create one (AUTH-5). */
export function OrganisationsPage() {
  const me = useMe();
  const qc = useQueryClient();
  const go = useGo();
  const [form, setForm] = useState({ name: "", display_name: "" });
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/orgs", {
          body: { name: form.name, display_name: form.display_name || undefined },
        }),
      ),
    onSuccess: (n) => {
      qc.invalidateQueries({ queryKey: meKey });
      go(`/${n.name}`);
    },
  });
  const errs = fieldErrors(create.error);
  const orgs = me.data?.orgs ?? [];
  return (
    <PageShell title="Organisations" subtitle="Organisations share Repos between their Members.">
      {orgs.length ? (
        <ul className="divide-y divide-line rounded-lg border border-line bg-surface">
          {orgs.map((o) => (
            <li key={o.name} className="flex items-center gap-2 px-4 py-2 text-sm">
              <AppLink href={`/${o.name}`} className="font-medium hover:underline">
                {o.display_name || o.name}
              </AppLink>
              <span className="font-mono text-xs text-muted">{o.name}</span>
              <Badge className="ml-auto">{o.role}</Badge>
            </li>
          ))}
        </ul>
      ) : (
        <EmptyState title="You're not in any Organisations" />
      )}
      <form
        className="max-w-md space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <h2 className="font-semibold">Create an Organisation</h2>
        <Field label="Name" hint="Used in Repo IDs, such as ml-team/model." error={errs.name}>
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
        {create.error && !errs.name ? <InlineError error={create.error} /> : null}
        <Button type="submit" variant="primary" disabled={!form.name || create.isPending}>
          Create Organisation
        </Button>
      </form>
    </PageShell>
  );
}
