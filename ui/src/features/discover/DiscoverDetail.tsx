import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { useEffect, useState } from "react";
import { api } from "@/api/client";
import type { DiscoverDetail as Detail } from "@/api/types";
import { AppLink } from "@/components/AppLink";
import { LazyMarkdown } from "@/components/content/LazyMarkdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox, Switch } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { Tabs } from "@/components/ui/tabs";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatCount } from "@/lib/format";
import { splitFrontMatter } from "@/lib/frontmatter";
import { unwrap } from "@/lib/problem";
import { filesForQuants, selectionBytes } from "@/lib/pull";
import { parseRepoId, repoPath } from "@/lib/repo";
import { ArsenalBadge } from "../repos/badges";

function PreviewTable({ preview }: { preview: Record<string, unknown> }) {
  const features = (preview.features ?? []) as { name: string }[];
  const rows = (preview.rows ?? []) as { row_idx: number; row: Record<string, unknown> }[];
  if (!features.length) return null;
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full text-xs">
        <caption className="sr-only">First rows</caption>
        <thead>
          <tr className="border-b border-line text-left">
            {features.map((f) => (
              <th key={f.name} scope="col" className="px-2 py-1.5 font-mono font-medium">
                {f.name}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.slice(0, 10).map((r) => (
            <tr key={r.row_idx} className="border-b border-line last:border-0">
              {features.map((f) => (
                <td key={f.name} className="max-w-64 truncate px-2 py-1.5">
                  {typeof r.row[f.name] === "object"
                    ? JSON.stringify(r.row[f.name])
                    : String(r.row[f.name])}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/**
 * The upstream detail panel (06 "Discover"): card, files, quant picker,
 * Configs and Splits, Dependencies, total size, and **Pull into arsenal**.
 */
export function DiscoverDetailPanel({
  type,
  id,
  upstream,
}: {
  type: "models" | "datasets";
  id: string;
  upstream: string;
}) {
  const [ns, name] = id.split("/") as [string, string];
  const toast = useToast();
  const qc = useQueryClient();
  const detail = useQuery({
    queryKey: ["discover", type, upstream, id],
    queryFn: async () =>
      unwrap(
        await api.GET("/discover/{type}/{ns}/{name}", {
          params: { path: { type, ns, name }, query: { upstream } },
        }),
      ),
  });
  const [quants, setQuants] = useState<string[]>([]);
  const [configs, setConfigs] = useState<string[]>([]);
  const [splits, setSplits] = useState<string[]>([]);
  const [parquetOnly, setParquetOnly] = useState(false);
  const [track, setTrack] = useState(false);
  const [tab, setTab] = useState("files");
  useEffect(() => {
    const q = detail.data?.quants;
    if (q?.length) {
      const preferred = q.find((g) => /Q4_K_M/i.test(g.name)) ?? q[Math.floor(q.length / 2)];
      setQuants(preferred ? [preferred.name] : []);
    }
  }, [detail.data?.quants]);
  const pull = useMutation({
    mutationFn: async (d: Detail) =>
      unwrap(
        await api.POST("/pulls", {
          body: {
            upstream,
            repo: d.id,
            type: d.type,
            track,
            selection: {
              ...(quants.length && d.quants?.length ? { quants } : {}),
              ...(configs.length ? { configs } : {}),
              ...(splits.length ? { splits } : {}),
              ...(parquetOnly ? { parquet_only: true } : {}),
            },
          },
        }),
      ),
    onSuccess: () => {
      toast(`Pull of ${id} started. Follow it in Jobs.`);
      qc.invalidateQueries({ queryKey: ["jobs"] });
      qc.invalidateQueries({ queryKey: ["discover"] });
    },
  });
  if (detail.isPending) return <LoadingRows rows={6} label="Loading details from the Upstream" />;
  if (detail.isError) return <ErrorState error={detail.error} onRetry={() => detail.refetch()} />;
  const d = detail.data;
  const sel = {
    quants: d.quants?.length ? quants : undefined,
    configs,
    splits,
    parquet_only: parquetOnly,
  };
  const bytes = selectionBytes(d, sel);
  const selectedPaths = d.quants?.length ? new Set(filesForQuants(d, quants)) : null;
  const local = parseRepoId(d.id, d.type);
  const card = d.card ? splitFrontMatter(d.card).body : "";
  const toggle = (list: string[], v: string) =>
    list.includes(v) ? list.filter((x) => x !== v) : [...list, v];
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <ArsenalBadge
          state={d.arsenal}
          updateAvailable={Boolean(d.local_sha && d.sha && d.local_sha !== d.sha)}
        />
        {d.gated ? <Badge tone="warn">Gated upstream</Badge> : null}
        {d.pipeline_tag ? <Badge>{d.pipeline_tag}</Badge> : null}
        {d.library_name ? <Badge>{d.library_name}</Badge> : null}
        {local && d.arsenal !== "none" ? (
          <AppLink
            href={repoPath(local.kind, local.ns, local.name)}
            className="text-sm text-accent underline"
          >
            Open in your arsenal
          </AppLink>
        ) : null}
      </div>
      {d.quants?.length ? (
        <fieldset className="space-y-2">
          <legend className="text-sm font-semibold">Quantisations</legend>
          <p className="text-xs text-muted">
            Pick the quants to pull. Projector files for vision models come with any quant.
          </p>
          <ul className="grid gap-1.5 sm:grid-cols-2">
            {d.quants.map((q) => (
              <li key={q.name} className="rounded-md border border-line px-2 py-1.5">
                <Checkbox
                  checked={quants.includes(q.name)}
                  onChange={() => setQuants(toggle(quants, q.name))}
                  label={
                    <span className="flex items-baseline gap-2">
                      <span className="font-mono font-medium">{q.name}</span>
                      <span className="tabular font-mono text-xs text-muted">
                        {formatBytes(q.size)}
                      </span>
                      {q.present ? <Badge tone="ok">In arsenal</Badge> : null}
                      {q.files.length > 1 ? (
                        <span className="text-xs text-muted">{q.files.length} files</span>
                      ) : null}
                    </span>
                  }
                />
              </li>
            ))}
          </ul>
        </fieldset>
      ) : null}
      {d.configs?.length ? (
        <fieldset className="space-y-2">
          <legend className="text-sm font-semibold">Configs and Splits</legend>
          <p className="text-xs text-muted">Pick none to pull everything.</p>
          <ul className="space-y-1.5">
            {d.configs.map((c) => (
              <li key={c.config} className="rounded-md border border-line px-2 py-1.5">
                <Checkbox
                  checked={configs.includes(c.config)}
                  onChange={() => setConfigs(toggle(configs, c.config))}
                  label={<span className="font-mono">{c.config}</span>}
                />
                <div className="mt-1 flex flex-wrap gap-3 pl-6">
                  {c.splits.map((s) => (
                    <Checkbox
                      key={s.split}
                      checked={splits.includes(s.split)}
                      onChange={() => setSplits(toggle(splits, s.split))}
                      label={
                        <span className="text-xs">
                          <span className="font-mono">{s.split}</span>{" "}
                          <span className="text-muted">
                            {s.num_rows !== undefined ? `${formatCount(s.num_rows)} rows` : ""}
                            {s.num_bytes_parquet
                              ? `, ${formatBytes(s.num_bytes_parquet)} Parquet`
                              : ""}
                          </span>
                        </span>
                      }
                    />
                  ))}
                </div>
              </li>
            ))}
          </ul>
          <Checkbox
            label="Parquet export only"
            hint="Pulls the Upstream's Parquet export of the chosen Splits instead of the source files, which is often far smaller."
            checked={parquetOnly}
            onChange={(e) => setParquetOnly(e.target.checked)}
          />
        </fieldset>
      ) : null}
      {d.dependencies.length ? (
        <section aria-labelledby="dd-deps" className="space-y-1">
          <h3 id="dd-deps" className="text-sm font-semibold">
            Dependencies
          </h3>
          <ul className="space-y-1 text-sm">
            {d.dependencies.map((dep) => (
              <li key={dep.repo} className="flex flex-wrap items-center gap-1.5">
                <span className="font-mono">{dep.repo}</span>
                {dep.required ? (
                  <Badge tone="info">Pulled with it</Badge>
                ) : (
                  <Badge>Suggested</Badge>
                )}
                <ArsenalBadge state={dep.arsenal} />
              </li>
            ))}
          </ul>
        </section>
      ) : null}
      <div className="sticky bottom-0 -mx-5 space-y-3 border-t border-line bg-surface px-5 py-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="tabular text-sm">
            To download: <span className="font-mono font-semibold">{formatBytes(bytes)}</span>
            <span className="text-muted"> of {formatBytes(d.total_size)}</span>
          </p>
          <Switch
            label="Keep it up to date"
            hint="Track the Upstream and pull new Commits on a schedule."
            checked={track}
            onCheckedChange={setTrack}
          />
        </div>
        <InlineError error={pull.error} />
        <Button
          variant="primary"
          size="lg"
          className="w-full"
          disabled={
            pull.isPending || pull.isSuccess || (Boolean(d.quants?.length) && quants.length === 0)
          }
          onClick={() => pull.mutate(d)}
        >
          <Download aria-hidden="true" />
          {pull.isSuccess ? "Pull started" : "Pull into arsenal"}
        </Button>
      </div>
      <Tabs
        label="Upstream Repo"
        value={tab}
        onValueChange={setTab}
        tabs={[
          {
            value: "files",
            label: `Files (${d.files.length})`,
            content: (
              <ul aria-label="Files" className="divide-y divide-line text-sm">
                {d.files.map((f) => (
                  <li key={f.path} className="flex items-center justify-between gap-2 py-1">
                    <span
                      className={`min-w-0 break-all font-mono text-xs ${selectedPaths && !selectedPaths.has(f.path) ? "text-muted line-through" : ""}`}
                    >
                      {f.path}
                    </span>
                    <span className="flex shrink-0 items-center gap-1.5">
                      {f.present ? <Badge tone="ok">In arsenal</Badge> : null}
                      <span className="tabular font-mono text-xs text-muted">
                        {formatBytes(f.size)}
                      </span>
                    </span>
                  </li>
                ))}
              </ul>
            ),
          },
          {
            value: "card",
            label: d.type === "dataset" ? "Dataset card" : "Model card",
            content: card ? (
              <LazyMarkdown>{card}</LazyMarkdown>
            ) : (
              <EmptyState title="No card on the Upstream" />
            ),
          },
          ...(d.preview
            ? [
                {
                  value: "preview",
                  label: "Preview",
                  content: <PreviewTable preview={d.preview} />,
                },
              ]
            : []),
        ]}
      />
    </div>
  );
}
