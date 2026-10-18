import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, UserPlus } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { AdminUser, Role } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { CopyField } from "@/components/ui/copy-button";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/form";
import { ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { type Column, DataTable } from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, relativeTime } from "@/lib/format";
import { errorMessage, fieldErrors, unwrap } from "@/lib/problem";
import { useSearchState } from "@/lib/use-search-state";

function LinkDialog({
  link,
  onClose,
  title,
}: {
  link: { url: string; expires_at: string } | null;
  onClose: () => void;
  title: string;
}) {
  return (
    <Dialog
      open={link !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={title}
      description="Hangar does not send email. Copy the link and send it yourself. It works once."
      footer={<Button onClick={onClose}>Done</Button>}
    >
      {link ? (
        <div className="space-y-2">
          <CopyField value={link.url} label="Copy the link" small />
          <p className="tabular text-xs text-muted">Expires {formatDateTime(link.expires_at)}.</p>
        </div>
      ) : null}
    </Dialog>
  );
}

function CreateUser({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    username: "",
    email: "",
    display_name: "",
    password: "",
    is_admin: false,
  });
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/admin/users", {
          body: { ...form, password: form.password || undefined, email: form.email || undefined },
        }),
      ),
    onSuccess: () => {
      setOpen(false);
      setForm({ username: "", email: "", display_name: "", password: "", is_admin: false });
      onDone();
    },
  });
  const errs = fieldErrors(create.error);
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title="Create a user"
      description="Leave the password empty and send a reset link instead, so only they know it."
      trigger={
        <Button variant="primary">
          <Plus aria-hidden="true" />
          Create user
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="create-user"
            disabled={!form.username || create.isPending}
          >
            Create user
          </Button>
        </>
      }
    >
      <form
        id="create-user"
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Username" error={errs.username}>
          {(p) => (
            <Input
              {...p}
              required
              className="font-mono"
              value={form.username}
              onChange={(e) => setForm({ ...form, username: e.target.value })}
            />
          )}
        </Field>
        <Field label="Email" error={errs.email}>
          {(p) => (
            <Input
              {...p}
              type="email"
              value={form.email}
              onChange={(e) => setForm({ ...form, email: e.target.value })}
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
        <Field label="Password" hint="Optional. At least 10 characters." error={errs.password}>
          {(p) => (
            <Input
              {...p}
              type="password"
              autoComplete="new-password"
              value={form.password}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
            />
          )}
        </Field>
        <Checkbox
          label="Instance admin"
          checked={form.is_admin}
          onChange={(e) => setForm({ ...form, is_admin: e.target.checked })}
        />
        {create.error && !Object.keys(errs).length ? <InlineError error={create.error} /> : null}
      </form>
    </Dialog>
  );
}

function Invite({ onLink }: { onLink: (l: { url: string; expires_at: string }) => void }) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ email: "", org: "", role: "read" as Role, is_admin: false });
  const invite = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/admin/invites", {
          body: {
            email: form.email || undefined,
            org: form.org || undefined,
            role: form.org ? form.role : undefined,
            is_admin: form.is_admin,
          },
        }),
      ),
    onSuccess: (l) => {
      setOpen(false);
      onLink(l);
    },
  });
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title="Invite someone"
      trigger={
        <Button>
          <UserPlus aria-hidden="true" />
          Create invite link
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button variant="primary" type="submit" form="invite" disabled={invite.isPending}>
            Create link
          </Button>
        </>
      }
    >
      <form
        id="invite"
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          invite.mutate();
        }}
      >
        <Field label="Email" hint="Optional, for your records.">
          {(p) => (
            <Input
              {...p}
              type="email"
              value={form.email}
              onChange={(e) => setForm({ ...form, email: e.target.value })}
            />
          )}
        </Field>
        <div className="flex gap-2">
          <Field label="Add to Organisation" hint="Optional." className="flex-1">
            {(p) => (
              <Input
                {...p}
                className="font-mono"
                value={form.org}
                onChange={(e) => setForm({ ...form, org: e.target.value })}
              />
            )}
          </Field>
          <Field label="Role">
            {(p) => (
              <Select
                {...p}
                className="w-auto"
                value={form.role}
                disabled={!form.org}
                onChange={(e) => setForm({ ...form, role: e.target.value as Role })}
              >
                <option value="read">Read</option>
                <option value="write">Write</option>
                <option value="admin">Admin</option>
              </Select>
            )}
          </Field>
        </div>
        <Checkbox
          label="Make them an Instance admin"
          checked={form.is_admin}
          onChange={(e) => setForm({ ...form, is_admin: e.target.checked })}
        />
        <InlineError error={invite.error} />
      </form>
    </Dialog>
  );
}

function AdminTokens() {
  const qc = useQueryClient();
  const tokens = useQuery({
    queryKey: ["admin", "tokens"],
    queryFn: async () =>
      unwrap(await api.GET("/admin/tokens", { params: { query: { limit: 200 } } })),
  });
  if (tokens.isPending) return <LoadingRows rows={2} label="Loading Tokens" />;
  if (tokens.isError) return <ErrorState error={tokens.error} onRetry={() => tokens.refetch()} />;
  return (
    <ul className="hg-enter divide-y divide-line rounded-lg border border-line bg-surface">
      {tokens.data.items.map((t) => (
        <li key={t.id} className="flex flex-wrap items-center gap-2 px-4 py-2.5 text-sm">
          <span className="font-mono">{t.username}</span>
          <span>{t.name}</span>
          <span className="font-mono text-xs text-muted">{t.prefix}...</span>
          <Badge mono>{t.scope}</Badge>
          <span className="tabular text-xs text-muted">
            {t.last_used_at ? `Used ${relativeTime(t.last_used_at)}` : "Never used"}
          </span>
          <ConfirmDialog
            trigger={
              <Button size="sm" variant="danger" className="ml-auto">
                Revoke
              </Button>
            }
            title={`Revoke ${t.username}'s Token ${t.name}?`}
            actionLabel="Revoke Token"
            onConfirm={async () => {
              const res = await api.DELETE("/admin/tokens/{id}", {
                params: { path: { id: t.id } },
              });
              if (!res.response.ok) unwrap(res);
              qc.invalidateQueries({ queryKey: ["admin", "tokens"] });
            }}
          >
            <p>Anything using it stops working at once.</p>
          </ConfirmDialog>
        </li>
      ))}
    </ul>
  );
}

/** Users: create, disable, make admin, reset links and invites; every Token (AUTH-1, AUTH-4, AUTH-6). */
export function AdminUsersPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const [search, update] = useSearchState<{ q?: string }>();
  const [link, setLink] = useState<{ url: string; expires_at: string } | null>(null);
  const [linkTitle, setLinkTitle] = useState("");
  const users = useInfiniteQuery({
    queryKey: ["admin", "users", search.q ?? ""],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/admin/users", {
          params: {
            query: {
              ...(search.q ? { q: search.q } : {}),
              ...(pageParam ? { cursor: pageParam } : {}),
            },
          },
        }),
      ),
    getNextPageParam: (l) => l.next_cursor || undefined,
  });
  const patch = useMutation({
    mutationFn: async ({
      id,
      body,
    }: {
      id: string;
      body: { disabled?: boolean; is_admin?: boolean };
    }) => unwrap(await api.PATCH("/admin/users/{id}", { params: { path: { id } }, body })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "users"] }),
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const reset = useMutation({
    mutationFn: async (u: AdminUser) =>
      unwrap(await api.POST("/admin/users/{id}/reset-link", { params: { path: { id: u.id } } })),
    onSuccess: (l, u) => {
      setLinkTitle(`Password reset link for ${u.username}`);
      setLink(l);
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const emailLink = useMutation({
    mutationFn: async (u: AdminUser) =>
      unwrap(
        await api.POST("/admin/users/{id}/email-link", {
          params: { path: { id: u.id } },
          // The address shown in the row: the link is bound to it, and a
          // change the user made since gives 409 instead.
          body: { email: u.pending_email ?? "" },
        }),
      ),
    onSuccess: (l, u) => {
      // Name the address the server bound the link to, not the row's copy.
      setLinkTitle(`Email confirmation link for ${u.username}, to send to ${l.email}`);
      setLink(l);
    },
    onError: (e) => {
      toast(errorMessage(e), "danger");
      qc.invalidateQueries({ queryKey: ["admin", "users"] });
    },
  });
  const items = users.data?.pages.flatMap((p) => p.items) ?? [];
  const columns: Column<AdminUser>[] = [
    {
      key: "user",
      header: "User",
      primary: true,
      cell: (u) => (
        <span className="flex flex-wrap items-center gap-1.5">
          <span className="font-mono font-medium wrap-anywhere">{u.username}</span>
          {u.display_name ? <span className="text-muted">{u.display_name}</span> : null}
          {u.is_admin ? <Badge tone="accent">Admin</Badge> : null}
          {u.disabled ? <Badge tone="danger">Disabled</Badge> : null}
          {u.oidc_providers?.map((p) => (
            <Badge key={p} tone="info">
              {p}
            </Badge>
          ))}
        </span>
      ),
    },
    {
      key: "email",
      header: "Email",
      cell: (u) => (
        <span className="flex flex-col text-xs wrap-anywhere">
          <span className={u.email ? undefined : "text-muted"}>{u.email || "Not set"}</span>
          {u.pending_email ? (
            <span className="text-muted">Waiting for confirmation: {u.pending_email}</span>
          ) : null}
        </span>
      ),
    },
    {
      key: "login",
      header: "Last sign-in",
      cell: (u) => (
        <span
          className="tabular text-xs"
          title={u.last_login_at ? formatDateTime(u.last_login_at) : undefined}
        >
          {u.last_login_at ? relativeTime(u.last_login_at) : "Never"}
        </span>
      ),
    },
    {
      key: "actions",
      header: <span className="sr-only">Actions</span>,
      className: "text-right",
      cell: (u) => (
        <span className="flex flex-wrap justify-end gap-1">
          {u.pending_email ? (
            <Button size="sm" variant="ghost" onClick={() => emailLink.mutate(u)}>
              Email link
            </Button>
          ) : null}
          <Button size="sm" variant="ghost" onClick={() => reset.mutate(u)}>
            Reset link
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => patch.mutate({ id: u.id, body: { is_admin: !u.is_admin } })}
          >
            {u.is_admin ? "Remove admin" : "Make admin"}
          </Button>
          <Button
            size="sm"
            variant={u.disabled ? "secondary" : "danger"}
            onClick={() => patch.mutate({ id: u.id, body: { disabled: !u.disabled } })}
          >
            {u.disabled ? "Enable" : "Disable"}
          </Button>
        </span>
      ),
    },
  ];
  return (
    <PageShell
      title="Users"
      actions={
        <>
          <Invite
            onLink={(l) => {
              setLinkTitle("Invite link");
              setLink(l);
            }}
          />
          <CreateUser onDone={() => qc.invalidateQueries({ queryKey: ["admin", "users"] })} />
        </>
      }
    >
      <LinkDialog link={link} title={linkTitle} onClose={() => setLink(null)} />
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
          aria-label="Search users"
          placeholder="Search by username or email"
          key={search.q ?? ""}
          defaultValue={search.q ?? ""}
        />
      </form>
      {users.isPending ? (
        <LoadingRows rows={4} label="Loading users" />
      ) : users.isError ? (
        <ErrorState error={users.error} onRetry={() => users.refetch()} />
      ) : (
        <DataTable
          className="hg-enter"
          caption="Users"
          rowHover
          columns={columns}
          rows={items}
          rowKey={(u) => u.id}
        />
      )}
      {users.hasNextPage ? <Button onClick={() => users.fetchNextPage()}>More users</Button> : null}
      <section aria-labelledby="all-tokens" className="space-y-2 pt-4">
        <h2 id="all-tokens" className="font-semibold">
          Every Token
        </h2>
        <AdminTokens />
      </section>
    </PageShell>
  );
}
