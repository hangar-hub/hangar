import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Activity } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import type { Job } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { ProgressBar } from "@/components/ui/progress";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { formatBytes, formatDuration, formatRate } from "@/lib/format";
import { isActive, jobTitle, statusLabel, statusTone } from "@/lib/jobs";
import { unwrap } from "@/lib/problem";

/** Polls the signed-in user's Jobs: every 2 seconds while one runs. */
export function useMyJobs(enabled: boolean) {
  return useQuery({
    queryKey: ["jobs", "mine"],
    enabled,
    queryFn: async () =>
      unwrap(await api.GET("/jobs", { params: { query: { mine: true, limit: 50 } } })),
    refetchInterval: (q) => (q.state.data?.items.some((j) => isActive(j.status)) ? 2000 : 15000),
  });
}

/** One Job with its progress, rate and ETA. */
export function JobRow({ job, onCancel }: { job: Job; onCancel?: () => void }) {
  const p = job.progress;
  return (
    <li className="space-y-1.5 py-3">
      <div className="flex items-start justify-between gap-3">
        <p className="min-w-0 break-words text-sm font-medium">{jobTitle(job)}</p>
        <Badge tone={statusTone(job.status)}>{statusLabel(job.status)}</Badge>
      </div>
      {p.bytes_total > 0 ? (
        <>
          <ProgressBar
            done={p.bytes_done}
            total={p.bytes_total}
            label={`Progress of ${jobTitle(job)}`}
          />
          <p className="tabular font-mono text-xs text-muted">
            {formatBytes(p.bytes_done)} of {formatBytes(p.bytes_total)}
            {job.status === "running"
              ? ` · ${formatRate(p.rate)} · ${formatDuration(p.eta_seconds)} left`
              : ""}
            {p.files_total > 0 ? ` · ${p.files_done} of ${p.files_total} files` : ""}
          </p>
        </>
      ) : null}
      {p.message ? <p className="text-xs text-muted">{p.message}</p> : null}
      {job.error ? <p className="text-xs text-danger">{job.error}</p> : null}
      {onCancel && isActive(job.status) ? (
        <Button size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      ) : null}
    </li>
  );
}

/** The top bar's Jobs button: running count, and a drawer with progress. */
export function JobsIndicator() {
  const [open, setOpen] = useState(false);
  const jobs = useMyJobs(true);
  const qc = useQueryClient();
  const cancel = useMutation({
    mutationFn: async (id: string) =>
      unwrap(await api.POST("/jobs/{id}/cancel", { params: { path: { id } } })),
    onSettled: () => qc.invalidateQueries({ queryKey: ["jobs"] }),
  });
  const active = jobs.data?.items.filter((j) => isActive(j.status)) ?? [];
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      side="right"
      title="Jobs"
      description="Your Pulls, uploads and other background work."
      trigger={
        <Button
          variant="ghost"
          size="sm"
          aria-label={`Jobs, ${active.length} running`}
          className="relative"
        >
          <Activity aria-hidden="true" />
          <span className="tabular hidden sm:inline">Jobs</span>
          {active.length > 0 ? (
            <span className="tabular rounded-sm bg-accent-fill px-1 text-[11px] font-semibold text-accent-ink">
              {active.length}
            </span>
          ) : null}
        </Button>
      }
    >
      {jobs.isPending ? (
        <LoadingRows rows={3} label="Loading Jobs" />
      ) : jobs.isError ? (
        <ErrorState error={jobs.error} onRetry={() => jobs.refetch()} />
      ) : jobs.data.items.length === 0 ? (
        <EmptyState title="No Jobs yet">
          Pulls you start from{" "}
          <Link to="/discover" className="text-accent underline" onClick={() => setOpen(false)}>
            Discover
          </Link>{" "}
          show their progress here.
        </EmptyState>
      ) : (
        <ul className="divide-y divide-line">
          {jobs.data.items.map((j) => (
            <JobRow key={j.id} job={j} onCancel={() => cancel.mutate(j.id)} />
          ))}
        </ul>
      )}
    </Dialog>
  );
}
