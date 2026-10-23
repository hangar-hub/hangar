import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Scope, Token, TokenCreated } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { ScrollRegion } from "@/components/ScrollRegion";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyField } from "@/components/ui/copy-button";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/form";
import { InfoBadge } from "@/components/ui/info-badge";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { type Column, DataTable } from "@/components/ui/table";
import { formatDate, relativeTime } from "@/lib/format";
import { fieldErrors, unwrap } from "@/lib/problem";

const expiries = [
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "1 year" },
  { value: "", label: "Never" },
];

/** The expiry time for a number of days from now, or undefined for never. */
export function expiryFrom(days: string, now: Date = new Date()): string | undefined {
  if (!days) return undefined;
  return new Date(now.getTime() + Number(days) * 86_400_000).toISOString();
}

function list(s: string): string[] {
  return s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);
}

// What each scope allows on top of the user's own permissions, as
// docs/concepts/permissions.md says: a write Token also does what a write
// Token does on huggingface.co through huggingface_hub (delete, rename within
// the Namespace, visibility and gating) on Repos the user is an admin of, and
// the admin scope covers every other Repo administration, Organisation
// Members and the Instance.
export const scopeHelp: Record<Scope, string> = {
  read: "Download and clone.",
  write:
    "Also upload, push and start Pulls, and, through huggingface_hub, delete, rename or change the visibility and gating of Repos you administer.",
  admin:
    "Also administer Repos in Hangar, such as webhooks, collaborators and transfers, and manage Organisation Members and, for Instance admins, the Instance.",
};

function CreateToken({ onCreated }: { onCreated: (t: TokenCreated) => void }) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    name: "",
    scope: "read" as Scope,
    expiry: "90",
    namespaces: "",
    repos: "",
  });
  const create = useMutation({
    mutationFn: async () => {
      const namespaces = list(form.namespaces);
      const repos = list(form.repos);
      return unwrap(
        await api.POST("/me/tokens", {
          body: {
            name: form.name,
            scope: form.scope,
            expires_at: expiryFrom(form.expiry),
            ...(namespaces.length || repos.length
              ? {
                  restrictions: {
                    ...(namespaces.length ? { namespaces } : {}),
                    ...(repos.length ? { repos } : {}),
                  },
                }
              : {}),
          },
        }),
      );
    },
    onSuccess: (t) => {
      setOpen(false);
      setForm({ name: "", scope: "read", expiry: "90", namespaces: "", repos: "" });
      onCreated(t);
    },
  });
  const errs = fieldErrors(create.error);
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title="Create a Token"
      description="Use it as a bearer token with the Hub API, or as the password for git."
      trigger={
        <Button variant="primary">
          <Plus aria-hidden="true" />
          Create Token
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="create-token"
            disabled={!form.name.trim() || create.isPending}
          >
            Create Token
          </Button>
        </>
      }
    >
      <form
        id="create-token"
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Name" hint="Where you'll use it, such as laptop or CI." error={errs.name}>
          {(p) => (
            <Input
              {...p}
              required
              maxLength={100}
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
          )}
        </Field>
        <fieldset className="space-y-1.5">
          <legend className="text-sm font-medium">Scope</legend>
          {(["read", "write", "admin"] as Scope[]).map((s) => (
            <label
              key={s}
              className="flex cursor-pointer items-start gap-2 rounded-md border border-line px-3 py-2 text-sm transition-[border-color,background-color] duration-150 hover:border-line-strong has-[:checked]:border-accent/50 has-[:checked]:bg-accent-soft/50"
            >
              <input
                type="radio"
                name="scope"
                className="mt-0.5 size-4 shrink-0 accent-[var(--hg-accent-fill)]"
                checked={form.scope === s}
                onChange={() => setForm({ ...form, scope: s })}
              />
              <span>
                <span className="font-mono">{s}</span>
                <span className="block text-xs text-muted">{scopeHelp[s]}</span>
              </span>
            </label>
          ))}
        </fieldset>
        <Field label="Expires after">
          {(p) => (
            <Select
              {...p}
              value={form.expiry}
              onChange={(e) => setForm({ ...form, expiry: e.target.value })}
            >
              {expiries.map((x) => (
                <option key={x.label} value={x.value}>
                  {x.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field
          label="Only these Namespaces"
          hint="Optional. Separate names with commas."
          error={errs["restrictions/namespaces"]}
        >
          {(p) => (
            <Input
              {...p}
              className="font-mono"
              placeholder="ada, ml-team"
              value={form.namespaces}
              onChange={(e) => setForm({ ...form, namespaces: e.target.value })}
            />
          )}
        </Field>
        <Field
          label="Only these Repos"
          hint="Optional. As type:namespace/name, such as model:ada/bert-tiny."
          error={errs["restrictions/repos"]}
        >
          {(p) => (
            <Input
              {...p}
              className="font-mono"
              placeholder="model:ada/bert-tiny"
              value={form.repos}
              onChange={(e) => setForm({ ...form, repos: e.target.value })}
            />
          )}
        </Field>
        {create.error && !Object.keys(errs).length ? <InlineError error={create.error} /> : null}
      </form>
    </Dialog>
  );
}

/** Tokens: create with scope, restrictions and expiry, shown once; revoke (AUTH-4). */
export function TokensPage() {
  const qc = useQueryClient();
  const [created, setCreated] = useState<TokenCreated | null>(null);
  const tokens = useQuery({
    queryKey: ["me", "tokens"],
    queryFn: async () => unwrap(await api.GET("/me/tokens")),
  });
  const columns: Column<Token>[] = [
    {
      key: "name",
      header: "Name",
      primary: true,
      cell: (t) => (
        <span className="flex flex-wrap items-center gap-1.5">
          <span className="font-medium wrap-anywhere">{t.name}</span>
          <span className="font-mono text-xs text-muted">{t.prefix}…</span>
        </span>
      ),
    },
    {
      key: "scope",
      header: "Scope",
      cell: (t) => (
        <InfoBadge
          mono
          explanation={{
            title: `The ${t.scope} scope`,
            body: <p>{scopeHelp[t.scope]}</p>,
            docs: "concepts/permissions/",
          }}
        >
          {t.scope}
        </InfoBadge>
      ),
    },
    {
      key: "limits",
      header: "Limited to",
      cell: (t) => (
        <span className="font-mono text-xs wrap-anywhere">
          {[...(t.restrictions?.namespaces ?? []), ...(t.restrictions?.repos ?? [])].join(", ") ||
            "Everything you can access"}
        </span>
      ),
    },
    {
      key: "used",
      header: "Last used",
      cell: (t) => (
        <span className="tabular text-xs">
          {t.last_used_at ? relativeTime(t.last_used_at) : "Never"}
        </span>
      ),
    },
    {
      key: "expires",
      header: "Expires",
      cell: (t) => (
        <span className="tabular whitespace-nowrap text-xs">
          {t.expires_at ? formatDate(t.expires_at) : "Never"}
        </span>
      ),
    },
    {
      key: "revoke",
      header: <span className="sr-only">Actions</span>,
      className: "w-24 text-right",
      cell: (t) => (
        <ConfirmDialog
          trigger={
            <Button size="sm" variant="danger">
              Revoke
            </Button>
          }
          title={`Revoke ${t.name}?`}
          actionLabel="Revoke Token"
          onConfirm={async () => {
            const res = await api.DELETE("/me/tokens/{id}", { params: { path: { id: t.id } } });
            if (!res.response.ok) unwrap(res);
            qc.invalidateQueries({ queryKey: ["me", "tokens"] });
          }}
        >
          <p>Anything that uses this Token stops working at once.</p>
        </ConfirmDialog>
      ),
    },
  ];
  return (
    <PageShell
      title="Tokens"
      subtitle="Tokens let the hf CLI, huggingface_hub, git and scripts act as you."
      actions={
        <CreateToken
          onCreated={(t) => {
            setCreated(t);
            qc.invalidateQueries({ queryKey: ["me", "tokens"] });
          }}
        />
      }
    >
      <Dialog
        open={created !== null}
        onOpenChange={(o) => {
          if (!o) setCreated(null);
        }}
        title="Copy your new Token"
        description="This is the only time Hangar shows it. Store it somewhere safe."
        persistent
        footer={<Button onClick={() => setCreated(null)}>I've copied it</Button>}
      >
        {created ? (
          <div className="space-y-3">
            <CopyField
              value={created.secret}
              label="Copy the Token"
              icon={<KeyRound aria-hidden="true" />}
              testId="token-secret"
            />
            <p className="text-sm text-muted">Sign in the hf CLI with it:</p>
            <ScrollRegion
              label="Sign-in commands"
              className="overflow-y-hidden rounded-md bg-raised"
            >
              <pre className="px-3 py-2 font-mono text-xs">
                export HF_ENDPOINT={window.location.origin}
                {"\n"}hf auth login --token {created.secret}
              </pre>
            </ScrollRegion>
          </div>
        ) : null}
      </Dialog>
      {tokens.isPending ? (
        <LoadingRows rows={3} label="Loading Tokens" />
      ) : tokens.isError ? (
        <ErrorState error={tokens.error} onRetry={() => tokens.refetch()} />
      ) : tokens.data.items.length === 0 ? (
        <EmptyState icon={<KeyRound />} title="No Tokens yet">
          Create one to use Hangar from the hf CLI, Python or git.
        </EmptyState>
      ) : (
        <DataTable
          className="hg-enter"
          caption="Your Tokens"
          columns={columns}
          rows={tokens.data.items}
          rowKey={(t) => t.id}
        />
      )}
    </PageShell>
  );
}
