import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Building2, Trash2, User } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Role } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { Tabs } from "@/components/ui/tabs";
import { useToast } from "@/components/ui/toast";
import { errorMessage, errorStatus, unwrap } from "@/lib/problem";
import { useMe } from "@/lib/session";
import { useSearchState } from "@/lib/use-search-state";
import { RepoCard } from "./repos/RepoCard";

function Members({ ns, canAdmin }: { ns: string; canAdmin: boolean }) {
  const qc = useQueryClient();
  const toast = useToast();
  const key = ["orgs", ns, "members"];
  const list = useQuery({
    queryKey: key,
    queryFn: async () => unwrap(await api.GET("/orgs/{ns}/members", { params: { path: { ns } } })),
  });
  const [user, setUser] = useState("");
  const [role, setRole] = useState<Role>("read");
  const set = useMutation({
    mutationFn: async ({ username, role }: { username: string; role: Role }) =>
      unwrap(
        await api.PUT("/orgs/{ns}/members/{user}", {
          params: { path: { ns, user: username } },
          body: { role },
        }),
      ),
    onSuccess: () => {
      setUser("");
      qc.invalidateQueries({ queryKey: key });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const remove = useMutation({
    mutationFn: async (username: string) => {
      const res = await api.DELETE("/orgs/{ns}/members/{user}", {
        params: { path: { ns, user: username } },
      });
      if (!res.response.ok) unwrap(res);
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: key }),
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  if (list.isPending) return <LoadingRows rows={3} label="Loading members" />;
  if (list.isError) return <ErrorState error={list.error} onRetry={() => list.refetch()} />;
  return (
    <div className="space-y-3">
      <ul className="divide-y divide-line rounded-lg border border-line bg-surface">
        {list.data.items.map((m) => (
          <li key={m.username} className="flex flex-wrap items-center gap-2 px-4 py-2 text-sm">
            <AppLink href={`/${m.username}`} className="font-mono hover:underline">
              {m.username}
            </AppLink>
            {m.display_name ? <span className="text-muted">{m.display_name}</span> : null}
            {m.source === "oidc" ? <Badge tone="info">From OIDC groups</Badge> : null}
            <span className="ml-auto flex items-center gap-1">
              {canAdmin && m.source === "manual" ? (
                <>
                  <Select
                    aria-label={`Role of ${m.username}`}
                    className="h-8 w-auto text-xs"
                    value={m.role}
                    onChange={(e) =>
                      set.mutate({ username: m.username, role: e.target.value as Role })
                    }
                  >
                    <option value="read">Read</option>
                    <option value="write">Write</option>
                    <option value="admin">Admin</option>
                  </Select>
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    aria-label={`Remove ${m.username}`}
                    onClick={() => remove.mutate(m.username)}
                  >
                    <Trash2 aria-hidden="true" />
                  </Button>
                </>
              ) : (
                <Badge>{m.role}</Badge>
              )}
            </span>
          </li>
        ))}
      </ul>
      {canAdmin ? (
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (user.trim()) set.mutate({ username: user.trim(), role });
          }}
        >
          <Field label="Add a member" className="min-w-48 flex-1">
            {(p) => (
              <Input
                {...p}
                placeholder="Username"
                value={user}
                onChange={(e) => setUser(e.target.value)}
              />
            )}
          </Field>
          <Field label="Role">
            {(p) => (
              <Select
                {...p}
                className="w-auto"
                value={role}
                onChange={(e) => setRole(e.target.value as Role)}
              >
                <option value="read">Read</option>
                <option value="write">Write</option>
                <option value="admin">Admin</option>
              </Select>
            )}
          </Field>
          <Button type="submit" disabled={!user.trim() || set.isPending}>
            Add member
          </Button>
        </form>
      ) : null}
      <InlineError error={set.error} />
    </div>
  );
}

/** `/{ns}`: a user's or Organisation's Repos, Collections and members. */
export function NamespacePage({ ns }: { ns: string }) {
  const me = useMe();
  const [search, update] = useSearchState<{ tab?: string }>();
  const n = useQuery({
    queryKey: ["namespace", ns],
    queryFn: async () => unwrap(await api.GET("/namespaces/{ns}", { params: { path: { ns } } })),
  });
  const repos = useQuery({
    queryKey: ["repos", { ns }],
    queryFn: async () => unwrap(await api.GET("/repos", { params: { query: { ns, limit: 100 } } })),
  });
  const collections = useQuery({
    queryKey: ["collections", "owner", ns],
    queryFn: async () =>
      unwrap(await api.GET("/collections", { params: { query: { owner: ns } } })),
  });
  if (n.isPending) return <LoadingRows rows={4} label="Loading the profile" />;
  if (n.isError) {
    if (errorStatus(n.error) === 404) {
      return (
        <PageShell title="Not found">
          <EmptyState title={`There is no user or Organisation called ${ns}`}>
            Check the name in the address.
          </EmptyState>
        </PageShell>
      );
    }
    return <ErrorState error={n.error} onRetry={() => n.refetch()} />;
  }
  const x = n.data;
  const isOrg = x.kind === "org";
  const canAdmin = Boolean(
    me.data?.is_admin || me.data?.orgs.some((o) => o.name === x.name && o.role === "admin"),
  );
  const tab = search.tab ?? "repos";
  return (
    <PageShell
      title={
        <span className="flex items-center gap-2">
          {isOrg ? (
            <Building2 className="size-6 text-muted" aria-hidden="true" />
          ) : (
            <User className="size-6 text-muted" aria-hidden="true" />
          )}
          {x.display_name || x.name}
        </span>
      }
      subtitle={
        <span className="flex flex-wrap items-center gap-2">
          <span className="font-mono">{x.name}</span>
          <Badge>{isOrg ? "Organisation" : "User"}</Badge>
          {x.reserved_mirror ? <Badge tone="info">Holds Mirrors</Badge> : null}
        </span>
      }
    >
      {x.description ? <p className="max-w-prose">{x.description}</p> : null}
      <Tabs
        label="Profile"
        value={tab}
        onValueChange={(v) => update({ tab: v === "repos" ? undefined : v })}
        tabs={[
          {
            value: "repos",
            label: `Repos${x.repo_count !== undefined ? ` (${x.repo_count})` : ""}`,
            content: repos.isPending ? (
              <LoadingRows rows={3} label="Loading Repos" />
            ) : repos.isError ? (
              <ErrorState error={repos.error} onRetry={() => repos.refetch()} />
            ) : repos.data.items.length === 0 ? (
              <EmptyState title="No Repos you can see" />
            ) : (
              <>
                {/* Each card has an h3 title, so the grid needs an h2 above it. */}
                <h2 id="namespace-repos" className="sr-only">
                  Repos
                </h2>
                <ul
                  aria-labelledby="namespace-repos"
                  className="grid gap-3 md:grid-cols-2 xl:grid-cols-3"
                >
                  {repos.data.items.map((r) => (
                    <li key={`${r.type}/${r.id}`}>
                      <RepoCard repo={r} />
                    </li>
                  ))}
                </ul>
              </>
            ),
          },
          {
            value: "collections",
            label: "Collections",
            content: collections.data?.items.length ? (
              <ul className="space-y-2">
                {collections.data.items.map((c) => (
                  <li key={c.slug}>
                    <AppLink
                      href={`/collections/${c.namespace}/${c.slug}`}
                      className="font-medium hover:underline"
                    >
                      {c.title}
                    </AppLink>
                    <span className="ml-2 text-xs text-muted">{c.items.length} items</span>
                  </li>
                ))}
              </ul>
            ) : (
              <EmptyState title="No Collections" />
            ),
          },
          ...(isOrg
            ? [
                {
                  value: "members",
                  label: `Members${x.member_count !== undefined ? ` (${x.member_count})` : ""}`,
                  content: <Members ns={x.name} canAdmin={canAdmin} />,
                },
              ]
            : []),
        ]}
      />
    </PageShell>
  );
}
