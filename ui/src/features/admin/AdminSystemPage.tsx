import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, XCircle } from "lucide-react";
import { api } from "@/api/client";
import type { Schemas } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows } from "@/components/ui/states";
import { unwrap } from "@/lib/problem";

/**
 * The Blob store backend and, on S3, how downloads and uploads reach the
 * bucket, for example "s3, redirect downloads, proxy uploads".
 */
export function blobStoreSummary(store: Schemas["SystemInfo"]["blob_store"]): string {
  const parts = [store.backend];
  if (store.download_mode) parts.push(`${store.download_mode} downloads`);
  if (store.upload_mode) parts.push(`${store.upload_mode} uploads`);
  return parts.join(", ");
}

/** Version, backends, telemetry and `doctor` results. */
export function AdminSystemPage() {
  const sys = useQuery({
    queryKey: ["admin", "system"],
    queryFn: async () => unwrap(await api.GET("/admin/system")),
  });
  return (
    <PageShell title="System" subtitle="This Instance's version, backends and health checks.">
      {sys.isPending ? (
        <LoadingRows rows={4} label="Loading system information" />
      ) : sys.isError ? (
        <ErrorState error={sys.error} onRetry={() => sys.refetch()} />
      ) : (
        <>
          <dl className="hg-enter grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            {[
              ["Version", `${sys.data.version}${sys.data.commit ? ` (${sys.data.commit})` : ""}`],
              ["Process role", sys.data.role],
              [
                "Database",
                `${sys.data.database.driver}, schema ${sys.data.database.schema_version}`,
              ],
              ["Blob store", blobStoreSummary(sys.data.blob_store)],
            ].map(([k, v]) => (
              <div key={k} className="rounded-lg border border-line bg-surface px-4 py-3">
                <dt className="text-xs text-muted">{k}</dt>
                <dd className="tabular mt-1 font-mono text-sm wrap-anywhere">{v}</dd>
              </div>
            ))}
          </dl>
          <section aria-labelledby="telemetry" className="hg-enter space-y-2">
            <h2 id="telemetry" className="font-semibold">
              Telemetry
            </h2>
            <div className="flex flex-wrap gap-2">
              {Object.entries(sys.data.telemetry).map(([signal, exporter]) => (
                <Badge key={signal} mono>
                  {signal}: {exporter}
                </Badge>
              ))}
            </div>
          </section>
          <section aria-labelledby="checks" className="hg-enter space-y-2">
            <h2 id="checks" className="font-semibold">
              Checks
            </h2>
            <ul className="divide-y divide-line rounded-lg border border-line bg-surface">
              {sys.data.checks.map((c) => (
                <li
                  key={c.name ?? c.message}
                  className="flex items-start gap-3 px-4 py-2.5 text-sm"
                >
                  {c.ok ? (
                    <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-ok" aria-hidden="true" />
                  ) : (
                    <XCircle className="mt-0.5 size-4 shrink-0 text-danger" aria-hidden="true" />
                  )}
                  <span className="sr-only">{c.ok ? "Passed:" : "Failed:"}</span>
                  <span className="shrink-0 pt-px font-mono text-xs">{c.name}</span>
                  <span className="min-w-0 flex-1 wrap-anywhere">{c.message}</span>
                  {c.latency_ms !== undefined ? (
                    <span className="tabular shrink-0 pt-px font-mono text-xs text-muted">
                      {c.latency_ms} ms
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          </section>
        </>
      )}
    </PageShell>
  );
}
