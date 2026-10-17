import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Trash2 } from "lucide-react";
import { type ReactNode, useEffect, useState } from "react";
import { api } from "@/api/client";
import type { Mirror, Repo, Visibility } from "@/api/types";
import { useGo } from "@/components/AppLink";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Checkbox, Field, Input, Select, Switch, Textarea } from "@/components/ui/form";
import { ErrorState, InlineError, LoadingRows, PermissionDenied } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatBytes, relativeTime } from "@/lib/format";
import { errorMessage, fieldErrors, unwrap } from "@/lib/problem";
import { apiType, type RepoKind, repoPath } from "@/lib/repo";
import { useMe } from "@/lib/session";
import { WebhookManager } from "../webhooks/WebhookManager";
import { DatasetsUsed } from "./DatasetsUsed";
import {
  type RepoRef,
  repoKey,
  repoPathParams,
  useDatasetConfigs,
  useQuants,
  useRefs,
  useRepo,
} from "./hooks";

function Section({
  title,
  description,
  children,
}: {
  title: string;
  description?: ReactNode;
  children: ReactNode;
}) {
  const id = `settings-${title.toLowerCase().replace(/\W+/g, "-")}`;
  return (
    <section
      aria-labelledby={id}
      className="grid gap-4 border-t border-line pt-6 md:grid-cols-[220px_minmax(0,1fr)]"
    >
      <div>
        <h2 id={id} className="font-semibold">
          {title}
        </h2>
        {description ? <p className="mt-1 text-sm text-muted">{description}</p> : null}
      </div>
      <div className="min-w-0 space-y-4">{children}</div>
    </section>
  );
}

function General({ repo, ref }: { repo: Repo; ref: RepoRef }) {
  const refs = useRefs(ref);
  const qc = useQueryClient();
  const toast = useToast();
  const [form, setForm] = useState({
    description: repo.description ?? "",
    tags: (repo.tags ?? []).join(", "),
    licence: repo.licence ?? "",
    visibility: repo.visibility,
    default_branch: repo.default_branch,
  });
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.PATCH("/repos/{type}/{ns}/{name}", {
          params: { path: repoPathParams(ref) },
          body: {
            description: form.description,
            tags: form.tags
              .split(",")
              .map((t) => t.trim())
              .filter(Boolean),
            licence: form.licence,
            visibility: form.visibility,
            default_branch: form.default_branch,
          },
        }),
      ),
    onSuccess: (r) => {
      qc.setQueryData(repoKey(ref), r);
      toast("Settings saved.");
    },
  });
  const errs = fieldErrors(save.error);
  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <Field label="Description" error={errs.description}>
        {(p) => (
          <Textarea
            {...p}
            rows={2}
            value={form.description}
            onChange={(e) => setForm({ ...form, description: e.target.value })}
          />
        )}
      </Field>
      <Field label="Tags" hint="Separate tags with commas." error={errs.tags}>
        {(p) => (
          <Input
            {...p}
            value={form.tags}
            onChange={(e) => setForm({ ...form, tags: e.target.value })}
          />
        )}
      </Field>
      <Field label="Licence" hint="An SPDX ID such as apache-2.0, or other." error={errs.licence}>
        {(p) => (
          <Input
            {...p}
            className="font-mono"
            value={form.licence}
            onChange={(e) => setForm({ ...form, licence: e.target.value })}
          />
        )}
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          label="Visibility"
          hint="Public: anyone who can reach Hangar. Internal: anyone signed in. Private: Namespace members and collaborators."
          error={errs.visibility}
        >
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
        <Field label="Default branch" error={errs.default_branch}>
          {(p) => (
            <Select
              {...p}
              className="font-mono"
              value={form.default_branch}
              onChange={(e) => setForm({ ...form, default_branch: e.target.value })}
            >
              {(refs.data?.branches ?? [{ name: repo.default_branch }]).map((b) => (
                <option key={b.name} value={b.name}>
                  {b.name}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </div>
      {save.error && !Object.keys(errs).length ? <InlineError error={save.error} /> : null}
      <Button type="submit" variant="primary" disabled={save.isPending}>
        Save settings
      </Button>
    </form>
  );
}

function Gating({ repo, ref }: { repo: Repo; ref: RepoRef }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [mode, setMode] = useState<"" | "auto" | "manual">(repo.gated);
  const [prompt, setPrompt] = useState("");
  const requests = useQuery({
    queryKey: [...repoKey(ref), "access-requests"],
    enabled: repo.gated !== "",
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/access-requests", {
          params: { path: repoPathParams(ref), query: { status: "pending" } },
        }),
      ),
  });
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.PATCH("/repos/{type}/{ns}/{name}", {
          params: { path: repoPathParams(ref) },
          body: { gating: { mode, ...(prompt ? { prompt } : {}) } },
        }),
      ),
    onSuccess: (r) => {
      qc.setQueryData(repoKey(ref), r);
      toast(mode ? "Gating turned on." : "Gating turned off.");
    },
  });
  const decide = useMutation({
    mutationFn: async ({ id, status }: { id: string; status: "accepted" | "rejected" }) =>
      unwrap(
        await api.PATCH("/repos/{type}/{ns}/{name}/access-requests/{id}", {
          params: { path: { ...repoPathParams(ref), id } },
          body: { status },
        }),
      ),
    onSuccess: (a) => {
      toast(`${a.username}'s request ${a.status === "accepted" ? "accepted" : "rejected"}.`);
      qc.invalidateQueries({ queryKey: [...repoKey(ref), "access-requests"] });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  return (
    <>
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <Field
          label="Access"
          hint="With gating on, people must ask before they can download files. Automatic approval accepts every request."
        >
          {(p) => (
            <Select
              {...p}
              value={mode}
              onChange={(e) => setMode(e.target.value as "" | "auto" | "manual")}
            >
              <option value="">Not gated</option>
              <option value="auto">Gated, approve automatically</option>
              <option value="manual">Gated, approve by hand</option>
            </Select>
          )}
        </Field>
        {mode ? (
          <Field label="Message to people asking for access" hint="Optional.">
            {(p) => (
              <Textarea
                {...p}
                rows={2}
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
              />
            )}
          </Field>
        ) : null}
        <InlineError error={save.error} />
        <Button type="submit" disabled={save.isPending}>
          Save gating
        </Button>
      </form>
      {repo.gated ? (
        <div className="space-y-2">
          <h3 className="text-sm font-semibold">Pending requests</h3>
          {requests.isPending ? (
            <LoadingRows rows={1} label="Loading requests" />
          ) : requests.isError ? (
            <ErrorState error={requests.error} onRetry={() => requests.refetch()} />
          ) : requests.data.items.length === 0 ? (
            <p className="text-sm text-muted">No one is waiting for access.</p>
          ) : (
            <ul className="divide-y divide-line rounded-md border border-line">
              {requests.data.items.map((a) => (
                <li key={a.id} className="flex flex-wrap items-center gap-2 px-3 py-2 text-sm">
                  <span className="font-mono">{a.username}</span>
                  <span className="text-xs text-muted">asked {relativeTime(a.created_at)}</span>
                  {a.fields && Object.keys(a.fields).length ? (
                    <span className="w-full text-xs text-muted">
                      {Object.entries(a.fields)
                        .map(([k, v]) => `${k}: ${String(v)}`)
                        .join("; ")}
                    </span>
                  ) : null}
                  <span className="ml-auto flex gap-1">
                    <Button
                      size="sm"
                      variant="primary"
                      onClick={() => decide.mutate({ id: a.id, status: "accepted" })}
                    >
                      Accept
                    </Button>
                    <Button
                      size="sm"
                      onClick={() => decide.mutate({ id: a.id, status: "rejected" })}
                    >
                      Reject
                    </Button>
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}
    </>
  );
}

function Collaborators({ ref }: { ref: RepoRef }) {
  const qc = useQueryClient();
  const toast = useToast();
  const key = [...repoKey(ref), "collaborators"];
  const list = useQuery({
    queryKey: key,
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/collaborators", {
          params: { path: repoPathParams(ref) },
        }),
      ),
  });
  const [username, setUsername] = useState("");
  const [role, setRole] = useState<"read" | "write">("write");
  const put = useMutation({
    mutationFn: async (items: { username: string; role: "read" | "write" }[]) =>
      unwrap(
        await api.PUT("/repos/{type}/{ns}/{name}/collaborators", {
          params: { path: repoPathParams(ref) },
          body: { items },
        }),
      ),
    onSuccess: (d) => {
      qc.setQueryData(key, d);
      setUsername("");
      toast("Collaborators saved.");
    },
  });
  if (list.isPending) return <LoadingRows rows={2} label="Loading collaborators" />;
  if (list.isError) return <ErrorState error={list.error} onRetry={() => list.refetch()} />;
  const items = list.data.items;
  return (
    <div className="space-y-3">
      {items.length ? (
        <ul className="divide-y divide-line rounded-md border border-line">
          {items.map((c) => (
            <li key={c.username} className="flex items-center gap-2 px-3 py-2 text-sm">
              <span className="flex-1 font-mono">{c.username}</span>
              <Select
                aria-label={`Role of ${c.username}`}
                className="w-auto"
                value={c.role}
                onChange={(e) =>
                  put.mutate(
                    items.map((x) =>
                      x.username === c.username
                        ? { ...x, role: e.target.value as "read" | "write" }
                        : x,
                    ),
                  )
                }
              >
                <option value="read">Read</option>
                <option value="write">Write</option>
              </Select>
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={`Remove ${c.username}`}
                onClick={() => put.mutate(items.filter((x) => x.username !== c.username))}
              >
                <Trash2 aria-hidden="true" />
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted">
          No collaborators. Namespace members already have access.
        </p>
      )}
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (username.trim())
            put.mutate([
              ...items.filter((x) => x.username !== username.trim()),
              { username: username.trim(), role },
            ]);
        }}
      >
        <Field label="Username" className="min-w-40 flex-1">
          {(p) => <Input {...p} value={username} onChange={(e) => setUsername(e.target.value)} />}
        </Field>
        <Field label="Role">
          {(p) => (
            <Select
              {...p}
              className="w-auto"
              value={role}
              onChange={(e) => setRole(e.target.value as "read" | "write")}
            >
              <option value="read">Read</option>
              <option value="write">Write</option>
            </Select>
          )}
        </Field>
        <Button type="submit" disabled={!username.trim() || put.isPending}>
          Add collaborator
        </Button>
      </form>
      <InlineError error={put.error} />
    </div>
  );
}

function lines(s: string): string[] {
  return s
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
}

function MirrorSettings({ repo, ref }: { repo: Repo; ref: RepoRef }) {
  const qc = useQueryClient();
  const toast = useToast();
  const key = [...repoKey(ref), "mirror"];
  const mirror = useQuery({
    queryKey: key,
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/mirror", {
          params: { path: repoPathParams(ref) },
        }),
      ),
  });
  const quants = useQuants(
    ref,
    repo.type === "model" && Boolean(repo.summary?.formats?.includes("gguf")),
  );
  const configs = useDatasetConfigs(ref);
  const [m, setM] = useState<Mirror | null>(null);
  useEffect(() => {
    if (mirror.data && !m) setM(mirror.data);
  }, [mirror.data, m]);
  const save = useMutation({
    mutationFn: async (x: Mirror) =>
      unwrap(
        await api.PATCH("/repos/{type}/{ns}/{name}/mirror", {
          params: { path: repoPathParams(ref) },
          body: {
            selection: x.selection,
            track: x.track,
            track_ref: x.track_ref,
            schedule: x.schedule,
          },
        }),
      ),
    onSuccess: (x) => {
      qc.setQueryData(key, x);
      toast("Mirror settings saved.");
    },
  });
  if (mirror.isPending || !m) return <LoadingRows rows={3} label="Loading Mirror settings" />;
  if (mirror.isError) return <ErrorState error={mirror.error} onRetry={() => mirror.refetch()} />;
  const sel = m.selection ?? {};
  const setSel = (patch: Partial<typeof sel>) => setM({ ...m, selection: { ...sel, ...patch } });
  const toggle = (list: string[] | undefined, v: string) =>
    list?.includes(v) ? list.filter((x) => x !== v) : [...(list ?? []), v];
  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate(m);
      }}
    >
      <p className="text-sm text-muted">
        Follows <span className="font-mono text-ink">{m.upstream_repo}</span> on {m.upstream}.
        Status: {m.status}
        {m.last_error ? <span className="text-danger">. Last error: {m.last_error}</span> : null}
      </p>
      {quants.data?.items.length ? (
        <fieldset>
          <legend className="text-sm font-medium">Quants to keep</legend>
          <p className="text-xs text-muted">
            Hangar stores quant names, so a renamed file of the same quant is still followed.
          </p>
          <div className="mt-2 flex flex-wrap gap-3">
            {quants.data.items.map((q) => (
              <Checkbox
                key={q.name}
                label={
                  <span className="font-mono text-xs">
                    {q.name} <span className="text-muted">{formatBytes(q.size)}</span>
                  </span>
                }
                checked={Boolean(sel.quants?.includes(q.name))}
                onChange={() => setSel({ quants: toggle(sel.quants, q.name) })}
              />
            ))}
          </div>
        </fieldset>
      ) : null}
      {repo.type === "dataset" && configs.data?.configs.length ? (
        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">Configs and Splits to keep</legend>
          <div className="flex flex-wrap gap-3">
            {configs.data.configs.map((c) => (
              <Checkbox
                key={c.config}
                label={<span className="font-mono text-xs">{c.config}</span>}
                checked={Boolean(sel.configs?.includes(c.config))}
                onChange={() => setSel({ configs: toggle(sel.configs, c.config) })}
              />
            ))}
          </div>
          <div className="flex flex-wrap gap-3">
            {[...new Set(configs.data.configs.flatMap((c) => c.splits.map((s) => s.split)))].map(
              (s) => (
                <Checkbox
                  key={s}
                  label={<span className="font-mono text-xs">{s}</span>}
                  checked={Boolean(sel.splits?.includes(s))}
                  onChange={() => setSel({ splits: toggle(sel.splits, s) })}
                />
              ),
            )}
          </div>
          <Checkbox
            label="Parquet export only"
            checked={Boolean(sel.parquet_only)}
            onChange={(e) => setSel({ parquet_only: e.target.checked })}
          />
        </fieldset>
      ) : null}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Include" hint="Glob patterns, one per line. Empty means every file.">
          {(p) => (
            <Textarea
              {...p}
              rows={3}
              className="font-mono text-xs"
              value={(sel.include ?? []).join("\n")}
              onChange={(e) => setSel({ include: lines(e.target.value) })}
            />
          )}
        </Field>
        <Field label="Exclude" hint="Glob patterns, one per line.">
          {(p) => (
            <Textarea
              {...p}
              rows={3}
              className="font-mono text-xs"
              value={(sel.exclude ?? []).join("\n")}
              onChange={(e) => setSel({ exclude: lines(e.target.value) })}
            />
          )}
        </Field>
      </div>
      <Switch
        label="Track the Upstream"
        hint="Pull new Commits and new versions of the selected files on a schedule."
        checked={m.track}
        onCheckedChange={(v) => setM({ ...m, track: v })}
      />
      {m.track ? (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Branch to track">
            {(p) => (
              <Input
                {...p}
                className="font-mono"
                value={m.track_ref ?? ""}
                placeholder="main"
                onChange={(e) => setM({ ...m, track_ref: e.target.value })}
              />
            )}
          </Field>
          <Field label="Schedule" hint="Cron, in the Instance time zone. Default: every 6 hours.">
            {(p) => (
              <Input
                {...p}
                className="font-mono"
                value={m.schedule ?? ""}
                placeholder="0 */6 * * *"
                onChange={(e) => setM({ ...m, schedule: e.target.value })}
              />
            )}
          </Field>
        </div>
      ) : null}
      <InlineError error={save.error} />
      <Button type="submit" variant="primary" disabled={save.isPending}>
        Save Mirror settings
      </Button>
    </form>
  );
}

function DangerZone({ repo, ref }: { repo: Repo; ref: RepoRef }) {
  const go = useGo();
  const qc = useQueryClient();
  const toast = useToast();
  const me = useMe();
  const [newName, setNewName] = useState(repo.name);
  const [newNs, setNewNs] = useState("");
  const namespaces = [
    me.data?.username,
    ...(me.data?.orgs.filter((o) => o.role === "admin").map((o) => o.name) ?? []),
  ].filter((n): n is string => Boolean(n) && n !== repo.namespace);
  const moved = (r: Repo) => {
    qc.invalidateQueries({ queryKey: ["repos"] });
    go(`${repoPath(r.type, r.namespace, r.name)}/settings`, { replace: true });
  };
  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-end gap-2">
        <Field
          label="Rename"
          hint="Clients using the old ID are redirected."
          className="min-w-48 flex-1"
        >
          {(p) => (
            <Input
              {...p}
              className="font-mono"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
            />
          )}
        </Field>
        <ConfirmDialog
          danger={false}
          trigger={<Button disabled={!newName.trim() || newName === repo.name}>Rename</Button>}
          title={`Rename to ${repo.namespace}/${newName}?`}
          actionLabel="Rename"
          onConfirm={async () =>
            moved(
              unwrap(
                await api.POST("/repos/{type}/{ns}/{name}/rename", {
                  params: { path: repoPathParams(ref) },
                  body: { name: newName.trim() },
                }),
              ),
            )
          }
        >
          <p>The HTTP API, git and the UI redirect from the old ID.</p>
        </ConfirmDialog>
      </div>
      <div className="flex flex-wrap items-end gap-2">
        <Field label="Transfer to another Namespace" className="min-w-48 flex-1">
          {(p) => (
            <Select {...p} value={newNs} onChange={(e) => setNewNs(e.target.value)}>
              <option value="">Choose a Namespace</option>
              {namespaces.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <ConfirmDialog
          trigger={<Button disabled={!newNs}>Transfer</Button>}
          title={`Transfer to ${newNs}?`}
          actionLabel="Transfer"
          onConfirm={async () =>
            moved(
              unwrap(
                await api.POST("/repos/{type}/{ns}/{name}/transfer", {
                  params: { path: repoPathParams(ref) },
                  body: { namespace: newNs },
                }),
              ),
            )
          }
        >
          <p>
            Members of <span className="font-mono">{newNs}</span> get access, and the old ID
            redirects to{" "}
            <span className="font-mono">
              {newNs}/{repo.name}
            </span>
            .
          </p>
        </ConfirmDialog>
      </div>
      {repo.deleted_at ? (
        <Button
          variant="primary"
          onClick={async () => {
            try {
              const r = unwrap(
                await api.POST("/repos/{type}/{ns}/{name}/restore", {
                  params: { path: repoPathParams(ref) },
                }),
              );
              qc.setQueryData(repoKey(ref), r);
              toast("Repo restored.");
            } catch (e) {
              toast(errorMessage(e), "danger");
            }
          }}
        >
          Restore this Repo
        </Button>
      ) : (
        <ConfirmDialog
          trigger={<Button variant="danger">Delete this Repo</Button>}
          title={`Delete ${repo.id}?`}
          actionLabel="Delete Repo"
          confirmText={repo.id}
          onConfirm={async () => {
            const res = await api.DELETE("/repos/{type}/{ns}/{name}", {
              params: { path: repoPathParams(ref) },
            });
            if (!res.response.ok) unwrap(res);
            qc.invalidateQueries({ queryKey: ["repos"] });
            toast(`${repo.id} deleted. An admin can restore it until it is purged.`);
            go(repo.type === "dataset" ? "/datasets" : "/models");
          }}
        >
          <p>
            Clients stop seeing it at once. The Repo can be restored until the restore window ends,
            then its data is purged.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}

/** Repo settings for admins of the Repo (REPO-4, REPO-5, REPO-8, REPO-9, PULL-9). */
export function RepoSettingsPage({ kind, ns, name }: { kind: RepoKind; ns: string; name: string }) {
  const ref: RepoRef = { kind, ns, name };
  const repo = useRepo(ref);
  if (!repo.data) return null;
  const r = repo.data;
  if (!r.permissions?.admin)
    return <PermissionDenied>Only admins of this Repo can change its settings.</PermissionDenied>;
  return (
    <div className="space-y-6">
      <Section title="General">
        <General repo={r} ref={ref} />
      </Section>
      {kind === "model" && !r.mirror ? (
        <Section
          title="Datasets used"
          description={
            <>
              The datasets this model was trained or evaluated on, kept in the Model card's{" "}
              <code className="font-mono">datasets</code> metadata as on the Hugging Face Hub. Each
              dataset lists this model under Used by.
            </>
          }
        >
          <DatasetsUsed repo={r} />
        </Section>
      ) : null}
      {r.mirror ? (
        <Section
          title="Mirror"
          description="What this Mirror keeps and how it follows the Upstream."
        >
          <MirrorSettings repo={r} ref={ref} />
        </Section>
      ) : null}
      <Section
        title="Gated access"
        description="Ask people to request access before they download files."
      >
        <Gating repo={r} ref={ref} />
      </Section>
      <Section
        title="Collaborators"
        description="People outside the Namespace who can read or write this Repo."
      >
        <Collaborators ref={ref} />
      </Section>
      <Section
        title="Webhooks"
        description={
          <>
            Events for this Repo only. <Badge mono>{`${apiType(kind)}/${ns}/${name}`}</Badge>
          </>
        }
      >
        <WebhookManager scope={{ repo: `${apiType(kind)}/${ns}/${name}` }} title="Repo webhooks" />
      </Section>
      <Section title="Danger zone" description="Rename, move or delete the Repo.">
        <DangerZone repo={r} ref={ref} />
      </Section>
    </div>
  );
}
