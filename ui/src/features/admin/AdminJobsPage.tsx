import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "@/api/client";
import type { Job, JobStatus } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/form";
import { ProgressBar } from "@/components/ui/progress";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { type Column, DataTable } from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatDateTime, formatRate, relativeTime } from "@/lib/format";
import { isActive, jobTitle, kindLabel, statusLabel, statusTone } from "@/lib/jobs";
import { errorMessage, unwrap } from "@/lib/problem";
import { useSearchState } from "@/lib/use-search-state";

const kinds = [
  "pull",
  "pull.file",
  "mirror.sync",
  "index",
  "scan",
  "gc",
  "git.compact",
  "lfs.verify",
  "dataset.convert",
  "dataset.stats",
  "xet.gc",
  "webhook.deliver",
  "zip.export",
  "repo.purge",
  "upload.expire",
];
const statuses: JobStatus[] = ["pending", "running", "succeeded", "failed", "cancelled"];

/** Every Job on the Instance, with filters, cancel and retry (PULL-5, OPS-9). */
export function AdminJobsPage() {
  const [search, update] = useSearchState<{ kind?: string; status?: string }>();
  const qc = useQueryClient();
  const toast = useToast();
  const status = statuses.find((s) => s === search.status);
  const jobs = useInfiniteQuery({
    queryKey: ["jobs", "all", search.kind ?? "", status ?? ""],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/jobs", {
          params: {
            query: {
              ...(search.kind ? { kind: search.kind } : {}),
              ...(status ? { status } : {}),
              ...(pageParam ? { cursor: pageParam } : {}),
            },
          },
        }),
      ),
    getNextPageParam: (l) => l.next_cursor || undefined,
    refetchInterval: (q) =>
      q.state.data?.pages.some((p) => p.items.some((j) => isActive(j.status))) ? 2000 : 15000,
  });
  const act = useMutation({
    mutationFn: async ({ id, action }: { id: string; action: "cancel" | "retry" }) =>
      unwrap(
        action === "cancel"
          ? await api.POST("/jobs/{id}/cancel", { params: { path: { id } } })
          : await api.POST("/jobs/{id}/retry", { params: { path: { id } } }),
      ),
    onSuccess: (j) => {
      toast(j.status === "cancelled" ? "Job cancelled." : "Job queued again.");
      qc.invalidateQueries({ queryKey: ["jobs"] });
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  const items = jobs.data?.pages.flatMap((p) => p.items) ?? [];
  const columns: Column<Job>[] = [
    {
      key: "job",
      header: "Job",
      primary: true,
      cell: (j) => (
        <span className="block space-y-1">
          <span className="block font-medium">{jobTitle(j)}</span>
          {j.error ? <span className="block text-xs text-danger">{j.error}</span> : null}
          {j.progress.message ? (
            <span className="block text-xs text-muted">{j.progress.message}</span>
          ) : null}
        </span>
      ),
    },
    {
      key: "status",
      header: "Status",
      cell: (j) => <Badge tone={statusTone(j.status)}>{statusLabel(j.status)}</Badge>,
    },
    {
      key: "progress",
      header: "Progress",
      cell: (j) =>
        j.progress.bytes_total > 0 ? (
          <span className="block min-w-40 space-y-1">
            <ProgressBar
              done={j.progress.bytes_done}
              total={j.progress.bytes_total}
              label={`Progress of ${jobTitle(j)}`}
            />
            <span className="tabular font-mono text-xs text-muted">
              {formatBytes(j.progress.bytes_done)} of {formatBytes(j.progress.bytes_total)}
              {j.status === "running" ? `, ${formatRate(j.progress.rate)}` : ""}
            </span>
          </span>
        ) : (
          <span className="text-xs text-muted">
            {j.progress.files_total
              ? `${j.progress.files_done} of ${j.progress.files_total} files`
              : ""}
          </span>
        ),
    },
    {
      key: "who",
      header: "Started",
      cell: (j) => (
        <span className="text-xs" title={formatDateTime(j.created_at)}>
          {relativeTime(j.created_at)}
          {j.created_by ? ` by ${j.created_by}` : ""}, attempt {j.attempts} of {j.max_attempts}
        </span>
      ),
    },
    {
      key: "actions",
      header: <span className="sr-only">Actions</span>,
      className: "text-right",
      cell: (j) =>
        isActive(j.status) ? (
          <Button size="sm" onClick={() => act.mutate({ id: j.id, action: "cancel" })}>
            Cancel
          </Button>
        ) : j.status === "failed" || j.status === "cancelled" ? (
          <Button size="sm" onClick={() => act.mutate({ id: j.id, action: "retry" })}>
            Retry
          </Button>
        ) : null,
    },
  ];
  return (
    <PageShell title="Jobs">
      <div className="flex flex-wrap gap-2">
        <label className="flex items-center gap-2 text-sm">
          <span className="text-muted">Kind</span>
          <Select
            className="w-auto"
            value={search.kind ?? ""}
            onChange={(e) => update({ kind: e.target.value || undefined })}
          >
            <option value="">Any</option>
            {kinds.map((k) => (
              <option key={k} value={k}>
                {kindLabel(k)}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex items-center gap-2 text-sm">
          <span className="text-muted">Status</span>
          <Select
            className="w-auto"
            value={status ?? ""}
            onChange={(e) => update({ status: e.target.value || undefined })}
          >
            <option value="">Any</option>
            {statuses.map((s) => (
              <option key={s} value={s}>
                {statusLabel(s)}
              </option>
            ))}
          </Select>
        </label>
      </div>
      {jobs.isPending ? (
        <LoadingRows rows={5} label="Loading Jobs" />
      ) : jobs.isError ? (
        <ErrorState error={jobs.error} onRetry={() => jobs.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState title="No Jobs match" />
      ) : (
        <DataTable caption="Jobs" columns={columns} rows={items} rowKey={(j) => j.id} />
      )}
      {jobs.hasNextPage ? <Button onClick={() => jobs.fetchNextPage()}>More Jobs</Button> : null}
    </PageShell>
  );
}
