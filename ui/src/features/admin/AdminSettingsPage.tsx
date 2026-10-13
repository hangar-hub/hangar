import { useEffect, useState } from "react";
import type { InstanceSettings } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Select, Switch } from "@/components/ui/form";
import { ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { useInstanceSettings } from "./useSettings";

/** Instance settings: access, registration, sign-in, images, scans and webhooks. */
export function AdminSettingsPage() {
  const { settings, save } = useInstanceSettings();
  const [s, setS] = useState<InstanceSettings | null>(null);
  useEffect(() => {
    if (settings.data) setS(settings.data);
  }, [settings.data]);
  if (settings.isPending || !s) return <LoadingRows rows={5} label="Loading settings" />;
  if (settings.isError)
    return <ErrorState error={settings.error} onRetry={() => settings.refetch()} />;
  const set = (patch: InstanceSettings) => setS({ ...s, ...patch });
  return (
    <PageShell title="Instance settings">
      <form
        className="max-w-2xl divide-y divide-line"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate(s);
        }}
      >
        <Switch
          label="Anonymous read"
          hint="People who are not signed in can see and download public Repos."
          checked={Boolean(s.anonymous_read)}
          onCheckedChange={(v) => set({ anonymous_read: v })}
        />
        <div className="py-3">
          <Field label="Registration" hint="Who can create an account without an admin.">
            {(p) => (
              <Select
                {...p}
                className="max-w-xs"
                value={s.registration ?? "closed"}
                onChange={(e) =>
                  set({ registration: e.target.value as InstanceSettings["registration"] })
                }
              >
                <option value="closed">Closed: admins create accounts</option>
                <option value="invite">Invite only</option>
                <option value="open">Open: anyone can register</option>
              </Select>
            )}
          </Field>
        </div>
        <Switch
          label="Local sign-in"
          hint="Sign in with a username and password. When off, only OIDC works, except for the bootstrap admin."
          checked={Boolean(s.local_login)}
          onCheckedChange={(v) => set({ local_login: v })}
        />
        <Switch
          label="Load external images in cards"
          hint="Turn off for air-gapped Instances so pages never try to reach the internet."
          checked={Boolean(s.ui_external_images)}
          onCheckedChange={(v) => set({ ui_external_images: v })}
        />
        <Switch
          label="Block downloads of unsafe files"
          hint="Files with unsafe pickle imports need explicit confirmation in the UI and are refused to API clients."
          checked={Boolean(s.scan_block_unsafe_downloads)}
          onCheckedChange={(v) => set({ scan_block_unsafe_downloads: v })}
        />
        <Switch
          label="Allow webhooks to private addresses"
          hint="Turn off on multi-tenant Instances so webhooks cannot reach internal services."
          checked={Boolean(s.webhooks_allow_private_targets)}
          onCheckedChange={(v) => set({ webhooks_allow_private_targets: v })}
        />
        <div className="space-y-2 py-3">
          <h2 className="text-sm font-medium">OIDC providers</h2>
          <p className="text-xs text-muted">Set in the configuration file.</p>
          {s.oidc_providers?.length ? (
            <ul className="space-y-1 text-sm">
              {s.oidc_providers.map((p) => (
                <li key={p.id} className="flex flex-wrap items-center gap-2">
                  <Badge mono>{p.id}</Badge>
                  <span>{p.name}</span>
                  <span className="font-mono text-xs text-muted">{p.issuer}</span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm">None configured.</p>
          )}
        </div>
        <div className="space-y-2 pt-4">
          <InlineError error={save.error} />
          <Button type="submit" variant="primary" disabled={save.isPending}>
            Save settings
          </Button>
        </div>
      </form>
    </PageShell>
  );
}
