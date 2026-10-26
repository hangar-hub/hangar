import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { CheckResult, Upstream } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Switch } from "@/components/ui/form";
import { ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { errorMessage, fieldErrors, unwrap } from "@/lib/problem";
import { cn } from "@/lib/utils";

type Form = {
  name: string;
  display_name: string;
  base_url: string;
  viewer_url: string;
  namespace_prefix: string;
  instance_token: string;
  enabled: boolean;
};

function UpstreamDialog({ upstream, onDone }: { upstream?: Upstream; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<Form>({
    name: upstream?.name ?? "",
    display_name: upstream?.display_name ?? "",
    base_url: upstream?.base_url ?? "",
    viewer_url: upstream?.viewer_url ?? "",
    namespace_prefix: upstream?.namespace_prefix ?? "",
    instance_token: "",
    enabled: upstream?.enabled ?? true,
  });
  const save = useMutation({
    mutationFn: async () => {
      if (upstream) {
        const { name: _n, instance_token, ...rest } = form;
        return unwrap(
          await api.PATCH("/admin/upstreams/{id}", {
            params: { path: { id: upstream.id } },
            body: { ...rest, ...(instance_token ? { instance_token } : {}) },
          }),
        );
      }
      return unwrap(
        await api.POST("/admin/upstreams", {
          body: {
            ...form,
            instance_token: form.instance_token || undefined,
            viewer_url: form.viewer_url || undefined,
            namespace_prefix: form.namespace_prefix || undefined,
          },
        }),
      );
    },
    onSuccess: () => {
      setOpen(false);
      onDone();
    },
  });
  const errs = fieldErrors(save.error);
  const set = (k: keyof Form, v: string | boolean) => setForm({ ...form, [k]: v });
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title={upstream ? `Edit ${upstream.display_name}` : "Add an Upstream"}
      description="Any hub that speaks the Hugging Face Hub API, such as another Hangar Instance."
      trigger={
        upstream ? (
          <Button size="sm" variant="ghost">
            Edit
          </Button>
        ) : (
          <Button variant="primary">
            <Plus aria-hidden="true" />
            Add Upstream
          </Button>
        )
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button variant="primary" type="submit" form="upstream-form" disabled={save.isPending}>
            {upstream ? "Save" : "Add Upstream"}
          </Button>
        </>
      }
    >
      <form
        id="upstream-form"
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        {!upstream ? (
          <Field label="ID" hint="Short and lower case, such as corp." error={errs.name}>
            {(p) => (
              <Input
                {...p}
                required
                className="font-mono"
                value={form.name}
                onChange={(e) => set("name", e.target.value)}
              />
            )}
          </Field>
        ) : null}
        <Field label="Display name">
          {(p) => (
            <Input
              {...p}
              value={form.display_name}
              onChange={(e) => set("display_name", e.target.value)}
            />
          )}
        </Field>
        <Field label="Base URL" error={errs.base_url}>
          {(p) => (
            <Input
              {...p}
              type="url"
              required
              className="font-mono"
              value={form.base_url}
              onChange={(e) => set("base_url", e.target.value)}
            />
          )}
        </Field>
        <Field
          label="Dataset Viewer URL"
          hint='Optional. Defaults to the base URL with the host prefixed by "datasets-server.", such as https://datasets-server.huggingface.co.'
        >
          {(p) => (
            <Input
              {...p}
              type="url"
              className="font-mono"
              value={form.viewer_url}
              onChange={(e) => set("viewer_url", e.target.value)}
            />
          )}
        </Field>
        <Field
          label="Namespace prefix"
          hint="Optional. Mirrors get IDs such as corp-org/model, so they no longer match the Upstream's IDs."
        >
          {(p) => (
            <Input
              {...p}
              className="font-mono"
              value={form.namespace_prefix}
              onChange={(e) => set("namespace_prefix", e.target.value)}
            />
          )}
        </Field>
        <Field
          label="Instance token"
          hint={
            upstream?.has_instance_token
              ? "A token is saved. Enter a new one to replace it."
              : "Optional. Used for Pulls when a user has not saved their own token."
          }
        >
          {(p) => (
            <Input
              {...p}
              type="password"
              autoComplete="off"
              value={form.instance_token}
              onChange={(e) => set("instance_token", e.target.value)}
            />
          )}
        </Field>
        <Switch label="Enabled" checked={form.enabled} onCheckedChange={(v) => set("enabled", v)} />
        {save.error && !Object.keys(errs).length ? <InlineError error={save.error} /> : null}
      </form>
    </Dialog>
  );
}

/** Upstreams: add, enable, Instance tokens and connection tests (PULL-1). */
export function AdminUpstreamsPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const [results, setResults] = useState<Record<string, CheckResult>>({});
  const list = useQuery({
    queryKey: ["admin", "upstreams"],
    queryFn: async () => unwrap(await api.GET("/admin/upstreams")),
  });
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["admin", "upstreams"] });
    qc.invalidateQueries({ queryKey: ["upstreams"] });
  };
  const test = useMutation({
    mutationFn: async (id: string) =>
      unwrap(await api.POST("/admin/upstreams/{id}/test", { params: { path: { id } } })),
    onSuccess: (r, id) => setResults({ ...results, [id]: r }),
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const toggle = useMutation({
    mutationFn: async (u: Upstream) =>
      unwrap(
        await api.PATCH("/admin/upstreams/{id}", {
          params: { path: { id: u.id } },
          body: { enabled: !u.enabled },
        }),
      ),
    onSuccess: refresh,
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  return (
    <PageShell title="Upstreams" actions={<UpstreamDialog onDone={refresh} />}>
      {list.isPending ? (
        <LoadingRows rows={2} label="Loading Upstreams" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : (
        <ul className="hg-enter space-y-2">
          {list.data.items.map((u) => {
            const r = results[u.id];
            return (
              <li key={u.id} className="space-y-2 rounded-lg border border-line bg-surface p-4">
                <div className="flex flex-wrap items-center gap-2">
                  <h2 className="font-semibold">{u.display_name}</h2>
                  <span className="font-mono text-xs text-muted">{u.name}</span>
                  <span className="min-w-0 font-mono text-xs wrap-anywhere">{u.base_url}</span>
                  {u.source === "config" ? <Badge>From configuration</Badge> : null}
                  {u.has_instance_token ? <Badge tone="info">Instance token</Badge> : null}
                  {u.namespace_prefix ? <Badge mono>prefix {u.namespace_prefix}</Badge> : null}
                  <span className="ml-auto flex items-center gap-1">
                    <Switch
                      className="gap-0 py-0"
                      label={<span className="sr-only">Enabled</span>}
                      checked={u.enabled}
                      onCheckedChange={() => toggle.mutate(u)}
                    />
                    <Button size="sm" onClick={() => test.mutate(u.id)} disabled={test.isPending}>
                      Test connection
                    </Button>
                    <UpstreamDialog upstream={u} onDone={refresh} />
                    {u.source !== "config" ? (
                      <ConfirmDialog
                        trigger={
                          <Button
                            size="icon-sm"
                            variant="ghost"
                            aria-label={`Remove ${u.display_name}`}
                          >
                            <Trash2 aria-hidden="true" />
                          </Button>
                        }
                        title={`Remove ${u.display_name}?`}
                        actionLabel="Remove Upstream"
                        onConfirm={async () => {
                          const res = await api.DELETE("/admin/upstreams/{id}", {
                            params: { path: { id: u.id } },
                          });
                          if (!res.response.ok) unwrap(res);
                          refresh();
                        }}
                      >
                        <p>Mirrors from it stay in the Arsenal but can no longer sync.</p>
                      </ConfirmDialog>
                    ) : null}
                  </span>
                </div>
                {r ? (
                  <p
                    role="status"
                    className={cn(
                      "tabular text-sm wrap-anywhere",
                      r.ok ? "text-ok" : "text-danger",
                    )}
                  >
                    {r.ok ? "Connected. " : "Failed. "}
                    {r.message}
                    {r.latency_ms !== undefined ? ` (${r.latency_ms} ms)` : ""}
                  </p>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </PageShell>
  );
}
