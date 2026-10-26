import { useEffect, useState } from "react";
import type { InstanceSettings } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Switch, Textarea } from "@/components/ui/form";
import { ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { fieldErrors } from "@/lib/problem";
import { useInstanceSettings } from "./useSettings";

const MiB = 1024 * 1024;

/** Converts a bandwidth in MiB/s from the form to bytes per second (0 means unlimited). */
export function bandwidthBytes(mibPerSecond: string): number {
  const n = Number(mibPerSecond);
  return Number.isFinite(n) && n > 0 ? Math.round(n * MiB) : 0;
}

/** Pull windows such as 01:00-07:00, one per line. */
export function parseWindows(text: string): { windows: string[]; invalid: string[] } {
  const windows = text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
  const invalid = windows.filter((w) => {
    const m = /^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$/.exec(w);
    return !m;
  });
  return { windows, invalid };
}

/** Pull settings: bandwidth, concurrency, windows and Pull-through (PULL-7, PULL-10). */
export function AdminPullsPage() {
  const { settings, save } = useInstanceSettings();
  const [s, setS] = useState<InstanceSettings | null>(null);
  const [bandwidth, setBandwidth] = useState("");
  const [windows, setWindows] = useState("");
  const [allow, setAllow] = useState("");
  useEffect(() => {
    if (settings.data) {
      setS(settings.data);
      setBandwidth(
        settings.data.pull_bandwidth_limit ? String(settings.data.pull_bandwidth_limit / MiB) : "",
      );
      setWindows((settings.data.pull_windows ?? []).join("\n"));
      setAllow((settings.data.pull_through_allow ?? []).join("\n"));
    }
  }, [settings.data]);
  if (settings.isPending || !s) return <LoadingRows rows={5} label="Loading settings" />;
  if (settings.isError)
    return <ErrorState error={settings.error} onRetry={() => settings.refetch()} />;
  const w = parseWindows(windows);
  const errs = fieldErrors(save.error);
  return (
    <PageShell title="Pull settings" subtitle="Keep Pulls from saturating your connection.">
      <form
        className="max-w-2xl space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (w.invalid.length) return;
          save.mutate({
            pull_bandwidth_limit: bandwidthBytes(bandwidth),
            pull_concurrency: s.pull_concurrency,
            pull_windows: w.windows,
            pull_allow_override_windows: s.pull_allow_override_windows,
            pull_through_enabled: s.pull_through_enabled,
            pull_through_allow: allow
              .split("\n")
              .map((x) => x.trim())
              .filter(Boolean),
            pull_through_min_role: s.pull_through_min_role,
          });
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Bandwidth limit (MiB/s)"
            hint="Shared by every worker. Empty or 0 means no limit."
            error={errs.pull_bandwidth_limit}
          >
            {(p) => (
              <Input
                {...p}
                type="number"
                min={0}
                step="0.1"
                value={bandwidth}
                onChange={(e) => setBandwidth(e.target.value)}
              />
            )}
          </Field>
          <Field
            label="Files at once"
            hint="Across the whole Instance."
            error={errs.pull_concurrency}
          >
            {(p) => (
              <Input
                {...p}
                type="number"
                min={1}
                value={s.pull_concurrency ?? 4}
                onChange={(e) => setS({ ...s, pull_concurrency: Number(e.target.value) })}
              />
            )}
          </Field>
        </div>
        <Field
          label="Pull windows"
          hint="Times when Pulls may fetch files, one per line, such as 01:00-07:00, in the Instance time zone. Empty means any time."
          error={
            w.invalid.length
              ? `Not in the form 01:00-07:00: ${w.invalid.join(", ")}`
              : errs.pull_windows
          }
        >
          {(p) => (
            <Textarea
              {...p}
              rows={3}
              className="font-mono text-sm"
              value={windows}
              onChange={(e) => setWindows(e.target.value)}
            />
          )}
        </Field>
        <Switch
          label="Let writers pull now"
          hint="People with write access can start a Pull outside the windows. The bandwidth limit still applies."
          checked={Boolean(s.pull_allow_override_windows)}
          onCheckedChange={(v) => setS({ ...s, pull_allow_override_windows: v })}
        />
        <div className="space-y-3 border-t border-line pt-4">
          <Switch
            label="Pull-through"
            hint="A client asking for a Repo that is not in the Arsenal starts a Pull automatically."
            checked={Boolean(s.pull_through_enabled)}
            onCheckedChange={(v) => setS({ ...s, pull_through_enabled: v })}
          />
          {s.pull_through_enabled ? (
            <>
              <Field
                label="Allowed Repos"
                hint="Glob patterns of upstream Repo IDs, one per line, such as Qwen/* or *."
              >
                {(p) => (
                  <Textarea
                    {...p}
                    rows={3}
                    className="font-mono text-sm"
                    value={allow}
                    onChange={(e) => setAllow(e.target.value)}
                  />
                )}
              </Field>
              <Field label="Who can trigger it">
                {(p) => (
                  <Select
                    {...p}
                    className="max-w-xs"
                    value={s.pull_through_min_role ?? "user"}
                    onChange={(e) =>
                      setS({ ...s, pull_through_min_role: e.target.value as "user" | "anonymous" })
                    }
                  >
                    <option value="user">Signed-in users</option>
                    <option value="anonymous">Anyone, including anonymous clients</option>
                  </Select>
                )}
              </Field>
            </>
          ) : null}
        </div>
        {save.error && !Object.keys(errs).length ? <InlineError error={save.error} /> : null}
        <Button type="submit" variant="primary" disabled={save.isPending || w.invalid.length > 0}>
          Save Pull settings
        </Button>
      </form>
    </PageShell>
  );
}
