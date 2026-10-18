import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Trash } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { StorageUsage } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/form";
import { ProgressBar } from "@/components/ui/progress";
import { ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { type Column, DataTable } from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatCount, relativeTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";

const GiB = 1024 ** 3;

/** Bytes from a quota in GiB typed in the form; 0 removes the quota. */
export function quotaBytes(gib: string): number {
  const n = Number(gib);
  return Number.isFinite(n) && n > 0 ? Math.round(n * GiB) : 0;
}

type Row = StorageUsage["namespaces"][number];

function QuotaDialog({ row, onDone }: { row: Row; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [gib, setGib] = useState(row.quota_bytes ? String(row.quota_bytes / GiB) : "");
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.PUT("/admin/quotas/{ns}", {
          params: { path: { ns: row.namespace } },
          body: { bytes_limit: quotaBytes(gib) },
        }),
      ),
    onSuccess: () => {
      setOpen(false);
      onDone();
    },
  });
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title={`Quota for ${row.namespace}`}
      trigger={
        <Button size="sm" variant="ghost">
          Set quota
        </Button>
      }
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form={`quota-${row.namespace}`}
            disabled={save.isPending}
          >
            Save quota
          </Button>
        </>
      }
    >
      <form
        id={`quota-${row.namespace}`}
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
        className="space-y-3"
      >
        <Field
          label="Limit (GiB)"
          hint="Uploads and Pulls that would go over it are refused. Empty or 0 removes the quota."
        >
          {(p) => (
            <Input
              {...p}
              type="number"
              min={0}
              step="0.1"
              value={gib}
              onChange={(e) => setGib(e.target.value)}
            />
          )}
        </Field>
        <InlineError error={save.error} />
      </form>
    </Dialog>
  );
}

/** Storage usage by Namespace, quotas and garbage collection (OPS-4). */
export function AdminStoragePage() {
  const qc = useQueryClient();
  const toast = useToast();
  const usage = useQuery({
    queryKey: ["admin", "storage"],
    queryFn: async () => unwrap(await api.GET("/admin/storage")),
  });
  const refresh = () => qc.invalidateQueries({ queryKey: ["admin", "storage"] });
  const columns: Column<Row>[] = [
    {
      key: "ns",
      header: "Namespace",
      primary: true,
      cell: (r) => <span className="font-mono wrap-anywhere">{r.namespace}</span>,
    },
    {
      key: "repos",
      header: "Repos",
      cell: (r) => <span className="tabular">{formatCount(r.repo_count)}</span>,
    },
    {
      key: "lfs",
      header: "LFS objects",
      cell: (r) => <span className="tabular font-mono text-xs">{formatBytes(r.lfs_bytes)}</span>,
    },
    {
      key: "git",
      header: "git data",
      cell: (r) => <span className="tabular font-mono text-xs">{formatBytes(r.git_bytes)}</span>,
    },
    {
      key: "quota",
      header: "Quota",
      cell: (r) =>
        r.quota_bytes ? (
          <span className="block min-w-32 space-y-1">
            <span className="tabular font-mono text-xs">
              {formatBytes(r.lfs_bytes + r.git_bytes)} of {formatBytes(r.quota_bytes)}
            </span>
            <ProgressBar
              done={r.lfs_bytes + r.git_bytes}
              total={r.quota_bytes}
              label={`Quota used by ${r.namespace}`}
            />
          </span>
        ) : (
          <span className="text-xs text-muted">None</span>
        ),
    },
    {
      key: "set",
      header: <span className="sr-only">Actions</span>,
      className: "text-right",
      cell: (r) => <QuotaDialog row={r} onDone={refresh} />,
    },
  ];
  return (
    <PageShell
      title="Storage"
      actions={
        <ConfirmDialog
          danger={false}
          trigger={
            <Button>
              <Trash aria-hidden="true" />
              Run GC
            </Button>
          }
          title="Run garbage collection now?"
          actionLabel="Run GC"
          onConfirm={async () => {
            unwrap(await api.POST("/admin/gc"));
            toast("Garbage collection started. Follow it on the Jobs page.");
            qc.invalidateQueries({ queryKey: ["jobs"] });
          }}
        >
          <p>
            GC deletes LFS objects and git data that no Repo or upload has referenced for the grace
            period. It runs on a schedule as well.
          </p>
        </ConfirmDialog>
      }
    >
      {usage.isPending ? (
        <LoadingRows rows={4} label="Loading storage usage" />
      ) : usage.isError ? (
        <ErrorState error={usage.error} onRetry={() => usage.refetch()} />
      ) : (
        <>
          <dl className="hg-enter grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            {[
              ["LFS objects", formatBytes(usage.data.total_lfs_bytes)],
              ["git data", formatBytes(usage.data.total_git_bytes)],
              ["Xet upload cache", formatBytes(usage.data.xet_cache_bytes)],
              ["Last GC", usage.data.last_gc_at ? relativeTime(usage.data.last_gc_at) : "Never"],
            ].map(([k, v]) => (
              <div key={k} className="rounded-lg border border-line bg-surface px-4 py-3">
                <dt className="text-xs text-muted">{k}</dt>
                <dd className="tabular mt-1 font-mono text-lg">{v}</dd>
              </div>
            ))}
          </dl>
          <DataTable
            className="hg-enter"
            caption="Usage by Namespace"
            rowHover
            columns={columns}
            rows={[...usage.data.namespaces].sort(
              (a, b) => b.lfs_bytes + b.git_bytes - (a.lfs_bytes + a.git_bytes),
            )}
            rowKey={(r) => r.namespace}
          />
        </>
      )}
    </PageShell>
  );
}
