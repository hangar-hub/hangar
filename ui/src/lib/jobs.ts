import type { Job, JobStatus } from "@/api/types";

/** True while a Job is waiting or running. */
export function isActive(status: JobStatus): boolean {
  return status === "pending" || status === "running";
}

const kindLabels: Record<string, string> = {
  pull: "Pull",
  "pull.file": "Pull file",
  "mirror.sync": "Mirror sync",
  index: "Index",
  scan: "Scan",
  gc: "Garbage collection",
  "git.compact": "Compact git data",
  "lfs.verify": "Verify LFS objects",
  "dataset.convert": "Parquet export",
  "dataset.stats": "Dataset statistics",
  "xet.gc": "Xet cache clean-up",
  "webhook.deliver": "Webhook delivery",
  "zip.export": "Zip download",
  "repo.purge": "Purge deleted Repo",
  "upload.expire": "Expire uploads",
};

/** A readable name for a Job kind. */
export function kindLabel(kind: string): string {
  return kindLabels[kind] ?? kind;
}

/** The Repo a Job works on, from its payload, if it names one. */
export function jobRepo(job: Job): string | undefined {
  const p = job.payload ?? {};
  for (const key of ["repo", "repo_id", "upstream_repo"]) {
    const v = p[key];
    if (typeof v === "string" && v) return v;
  }
  return undefined;
}

/** A one-line title for a Job: its kind and the Repo it works on. */
export function jobTitle(job: Job): string {
  const repo = jobRepo(job);
  return repo ? `${kindLabel(job.kind)}: ${repo}` : kindLabel(job.kind);
}

const statusLabels: Record<JobStatus, string> = {
  pending: "Waiting",
  running: "Running",
  succeeded: "Done",
  failed: "Failed",
  cancelled: "Cancelled",
};

export function statusLabel(status: JobStatus): string {
  return statusLabels[status];
}

export function statusTone(status: JobStatus): "neutral" | "info" | "ok" | "danger" | "warn" {
  switch (status) {
    case "running":
      return "info";
    case "succeeded":
      return "ok";
    case "failed":
      return "danger";
    case "cancelled":
      return "warn";
    default:
      return "neutral";
  }
}
