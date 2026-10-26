import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api/client";
import type { Visibility } from "@/api/types";
import { useGo } from "@/components/AppLink";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/form";
import { InlineError, PermissionDenied, Skeleton } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { fieldErrors, unwrap } from "@/lib/problem";
import { type RepoKind, repoPath, treePath } from "@/lib/repo";
import { useMe, writableNamespaces } from "@/lib/session";
import { licences } from "../datasets/card";

/** Repo names the Hub accepts: letters, digits, '-', '_' and '.', up to 96 characters. */
export function validRepoName(name: string): boolean {
  return (
    /^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/.test(name) && !name.includes("..") && !name.endsWith(".git")
  );
}

/** `/new` and `/new-dataset`: Namespace, name, visibility, licence and an optional first upload. */
export function NewRepoPage({ kind }: { kind: RepoKind }) {
  const me = useMe();
  const go = useGo();
  const qc = useQueryClient();
  const toast = useToast();
  const namespaces = writableNamespaces(me.data);
  const [form, setForm] = useState({
    namespace: "",
    name: "",
    visibility: "public" as Visibility,
    licence: "",
    description: "",
    readme: true,
    upload: false,
  });
  const ns = form.namespace || namespaces[0] || "";
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos", {
          body: {
            type: kind,
            namespace: ns,
            name: form.name,
            visibility: form.visibility,
            licence: form.licence || undefined,
            description: form.description || undefined,
            readme: form.readme,
          },
        }),
      ),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["repos"] });
      toast(`Created ${r.id}.`);
      go(
        form.upload
          ? treePath(r.type, r.namespace, r.name, r.default_branch)
          : repoPath(r.type, r.namespace, r.name),
      );
    },
  });
  if (me.isPending) return <Skeleton className="h-96 max-w-xl" />;
  if (!me.data) return <PermissionDenied>Sign in to create a Repo.</PermissionDenied>;
  const errs = fieldErrors(create.error);
  const nameError =
    form.name && !validRepoName(form.name)
      ? "Use letters, digits, '-', '_' and '.', starting with a letter or digit."
      : errs.name;
  const what = kind === "dataset" ? "dataset" : "model";
  return (
    <PageShell
      title={`New ${what}`}
      subtitle={`Create a ${what} Repo. You can push to it with git, upload with the hf CLI or add files here.`}
    >
      <form
        className="max-w-xl space-y-5"
        onSubmit={(e) => {
          e.preventDefault();
          if (validRepoName(form.name)) create.mutate();
        }}
      >
        <div className="flex flex-wrap items-start gap-2">
          <Field label="Namespace" error={errs.namespace} className="w-48">
            {(p) => (
              <Select
                {...p}
                value={ns}
                onChange={(e) => setForm({ ...form, namespace: e.target.value })}
              >
                {namespaces.map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <span aria-hidden="true" className="mt-8 text-muted">
            /
          </span>
          <Field
            label={`${kind === "dataset" ? "Dataset" : "Model"} name`}
            error={nameError}
            className="min-w-48 flex-1"
          >
            {(p) => (
              <Input
                {...p}
                required
                className="font-mono"
                autoComplete="off"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            )}
          </Field>
        </div>
        <Field label="Description" hint="Optional. Shown on the Repo page.">
          {(p) => (
            <Textarea
              {...p}
              rows={2}
              value={form.description}
              onChange={(e) => setForm({ ...form, description: e.target.value })}
            />
          )}
        </Field>
        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">Visibility</legend>
          {(
            [
              ["public", "Public", "Anyone who can reach this Instance can see and download it."],
              ["internal", "Internal", "Anyone signed in to this Instance."],
              ["private", "Private", "Only Members of the Namespace and collaborators."],
            ] as const
          ).map(([v, label, hint]) => (
            <label key={v} className="flex items-start gap-2 text-sm">
              <input
                type="radio"
                name="visibility"
                value={v}
                checked={form.visibility === v}
                onChange={() => setForm({ ...form, visibility: v })}
                className="mt-0.5 size-4 accent-[var(--hg-accent-fill)]"
              />
              <span>
                <span className="font-medium">{label}</span>
                <span className="block text-xs text-muted">{hint}</span>
              </span>
            </label>
          ))}
        </fieldset>
        <Field label="Licence">
          {(p) => (
            <Select
              {...p}
              value={form.licence}
              onChange={(e) => setForm({ ...form, licence: e.target.value })}
            >
              <option value="">Not set</option>
              {licences.map((l) => (
                <option key={l} value={l}>
                  {l}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Checkbox
          label={`Add a ${kind === "dataset" ? "dataset card" : "Model card"} (README.md)`}
          checked={form.readme}
          onChange={(e) => setForm({ ...form, readme: e.target.checked })}
        />
        <Checkbox
          label="Upload files next"
          hint="Opens the Files tab with the upload panel."
          checked={form.upload}
          onChange={(e) => setForm({ ...form, upload: e.target.checked })}
        />
        {create.error && !Object.keys(errs).length ? <InlineError error={create.error} /> : null}
        <Button type="submit" variant="primary" size="lg" disabled={create.isPending || !form.name}>
          Create {what}
        </Button>
      </form>
    </PageShell>
  );
}
