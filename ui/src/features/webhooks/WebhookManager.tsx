import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Send, Trash2 } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Webhook } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Switch } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, relativeTime } from "@/lib/format";
import { errorMessage, fieldErrors, unwrap } from "@/lib/problem";

/** Every webhook event in 10-native-api.md. */
export const webhookEvents = [
  "repo.created",
  "repo.updated",
  "repo.deleted",
  "repo.restored",
  "repo.renamed",
  "repo.visibility_changed",
  "pull.started",
  "pull.completed",
  "pull.failed",
  "mirror.synced",
  "index.completed",
  "scan.completed",
  "discussion.created",
  "discussion.commented",
  "discussion.status_changed",
  "pr.merged",
];

function randomSecret(): string {
  const bytes = new Uint8Array(24);
  crypto.getRandomValues(bytes);
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function CreateWebhook({
  scope,
  onCreated,
}: {
  scope: { ns?: string; repo?: string };
  onCreated: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [url, setUrl] = useState("");
  const [events, setEvents] = useState<string[]>(["repo.updated"]);
  const [secret, setSecret] = useState(randomSecret);
  const create = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/webhooks", {
          body: {
            url,
            events,
            secret,
            enabled: true,
            ...(scope.ns ? { namespace: scope.ns } : {}),
            ...(scope.repo ? { repo: scope.repo } : {}),
          },
        }),
      ),
    onSuccess: () => {
      setOpen(false);
      setUrl("");
      setSecret(randomSecret());
      onCreated();
    },
  });
  const errs = fieldErrors(create.error);
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      wide
      title="Add a webhook"
      description="Hangar sends a signed POST to this URL for each event you choose."
      trigger={
        <Button size="sm" variant="primary">
          <Plus aria-hidden="true" />
          Add webhook
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="create-webhook"
            disabled={!url || events.length === 0 || create.isPending}
          >
            Add webhook
          </Button>
        </>
      }
    >
      <form
        id="create-webhook"
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="URL" error={errs.url}>
          {(p) => (
            <Input
              {...p}
              type="url"
              required
              placeholder="https://ci.example.com/hooks/hangar"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          )}
        </Field>
        <Field
          label="Secret"
          hint="Hangar signs each body with HMAC-SHA256 and this secret, in X-Hangar-Signature. Copy it now: it is not shown again."
        >
          {(p) => (
            <Input
              {...p}
              className="font-mono"
              required
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
            />
          )}
        </Field>
        <fieldset>
          <legend className="text-sm font-medium">Events</legend>
          <div className="mt-2 grid gap-1.5 sm:grid-cols-2">
            {webhookEvents.map((ev) => (
              <Checkbox
                key={ev}
                label={<span className="font-mono text-xs">{ev}</span>}
                checked={events.includes(ev)}
                onChange={(e) =>
                  setEvents(e.target.checked ? [...events, ev] : events.filter((x) => x !== ev))
                }
              />
            ))}
          </div>
        </fieldset>
        {create.error && !Object.keys(errs).length ? <InlineError error={create.error} /> : null}
      </form>
    </Dialog>
  );
}

function Deliveries({ hook }: { hook: Webhook }) {
  const qc = useQueryClient();
  const toast = useToast();
  const list = useQuery({
    queryKey: ["webhooks", hook.id, "deliveries"],
    queryFn: async () =>
      unwrap(
        await api.GET("/webhooks/{id}/deliveries", {
          params: { path: { id: hook.id }, query: { limit: 20 } },
        }),
      ),
  });
  const redeliver = useMutation({
    mutationFn: async (delivery: string) =>
      unwrap(
        await api.POST("/webhooks/{id}/deliveries/{delivery_id}/redeliver", {
          params: { path: { id: hook.id, delivery_id: delivery } },
        }),
      ),
    onSuccess: () => {
      toast("Delivery queued again.");
      qc.invalidateQueries({ queryKey: ["webhooks", hook.id, "deliveries"] });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  if (list.isPending) return <LoadingRows rows={2} label="Loading deliveries" />;
  if (list.isError) return <ErrorState error={list.error} onRetry={() => list.refetch()} />;
  if (!list.data.items.length) return <p className="text-sm text-muted">No deliveries yet.</p>;
  return (
    <ul aria-label="Recent deliveries" className="divide-y divide-line text-sm">
      {list.data.items.map((d) => {
        const ok = d.status_code !== undefined && d.status_code < 300;
        return (
          <li key={d.id} className="flex flex-wrap items-center gap-2 py-1.5">
            <Badge tone={d.status_code === undefined ? "neutral" : ok ? "ok" : "danger"} mono>
              {d.status_code ?? "Pending"}
            </Badge>
            <span className="font-mono text-xs">{d.event}</span>
            <span className="text-xs text-muted">
              attempt {d.attempt}, {relativeTime(d.created_at)}
              {d.duration_ms !== undefined ? `, ${d.duration_ms} ms` : ""}
            </span>
            {d.error ? <span className="text-xs text-danger">{d.error}</span> : null}
            {d.next_attempt_at ? (
              <span className="text-xs text-muted">
                Next try {formatDateTime(d.next_attempt_at)}
              </span>
            ) : null}
            <Button
              size="sm"
              variant="ghost"
              className="ml-auto"
              onClick={() => redeliver.mutate(d.id)}
            >
              Redeliver
            </Button>
          </li>
        );
      })}
    </ul>
  );
}

/**
 * Lists and manages webhooks for a Repo, a Namespace or, for admins, the
 * whole Instance (EVT-1).
 */
export function WebhookManager({
  scope,
  title = "Webhooks",
}: {
  scope: { ns?: string; repo?: string };
  title?: string;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const [open, setOpen] = useState<string | null>(null);
  const key = ["webhooks", scope.ns ?? "", scope.repo ?? ""];
  const list = useQuery({
    queryKey: key,
    queryFn: async () =>
      unwrap(
        await api.GET("/webhooks", {
          params: {
            query: {
              ...(scope.ns ? { ns: scope.ns } : {}),
              ...(scope.repo ? { repo: scope.repo } : {}),
            },
          },
        }),
      ),
  });
  const refresh = () => qc.invalidateQueries({ queryKey: ["webhooks"] });
  const toggle = useMutation({
    mutationFn: async (h: Webhook) =>
      unwrap(
        await api.PATCH("/webhooks/{id}", {
          params: { path: { id: h.id } },
          body: { enabled: !h.enabled },
        }),
      ),
    onSuccess: refresh,
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const test = useMutation({
    mutationFn: async (id: string) =>
      unwrap(await api.POST("/webhooks/{id}/test", { params: { path: { id } } })),
    onSuccess: (_d, id) => {
      toast("Test event queued.");
      qc.invalidateQueries({ queryKey: ["webhooks", id, "deliveries"] });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  return (
    <section aria-labelledby="webhooks-title" className="space-y-3">
      <div className="flex items-center justify-between gap-2">
        <h2 id="webhooks-title" className="text-base font-semibold">
          {title}
        </h2>
        <CreateWebhook scope={scope} onCreated={refresh} />
      </div>
      {list.isPending ? (
        <LoadingRows rows={2} label="Loading webhooks" />
      ) : list.isError ? (
        <ErrorState error={list.error} onRetry={() => list.refetch()} />
      ) : list.data.items.length === 0 ? (
        <EmptyState title="No webhooks">
          Add one to tell CI or chat about new Commits, Pulls and discussions.
        </EmptyState>
      ) : (
        <ul className="space-y-2">
          {list.data.items.map((h) => (
            <li key={h.id} className="rounded-lg border border-line bg-surface p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span className="min-w-0 flex-1 break-all font-mono text-sm">{h.url}</span>
                {h.namespace ? <Badge>{h.namespace}</Badge> : null}
                {h.repo ? <Badge mono>{h.repo}</Badge> : null}
                <Switch
                  label={<span className="sr-only">Enabled</span>}
                  checked={h.enabled}
                  onCheckedChange={() => toggle.mutate(h)}
                />
                <Button size="sm" variant="ghost" onClick={() => test.mutate(h.id)}>
                  <Send aria-hidden="true" />
                  Test
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-expanded={open === h.id}
                  onClick={() => setOpen(open === h.id ? null : h.id)}
                >
                  Deliveries
                </Button>
                <ConfirmDialog
                  trigger={
                    <Button
                      size="icon-sm"
                      variant="ghost"
                      aria-label={`Delete the webhook to ${h.url}`}
                    >
                      <Trash2 aria-hidden="true" />
                    </Button>
                  }
                  title="Delete this webhook?"
                  actionLabel="Delete webhook"
                  onConfirm={async () => {
                    const res = await api.DELETE("/webhooks/{id}", {
                      params: { path: { id: h.id } },
                    });
                    if (!res.response.ok) unwrap(res);
                    refresh();
                  }}
                >
                  <p>
                    Hangar stops sending events to <span className="font-mono">{h.url}</span>.
                  </p>
                </ConfirmDialog>
              </div>
              <p className="mt-1 font-mono text-xs text-muted">{h.events.join(", ")}</p>
              {open === h.id ? (
                <div className="mt-2 border-t border-line pt-2">
                  <Deliveries hook={h} />
                </div>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
