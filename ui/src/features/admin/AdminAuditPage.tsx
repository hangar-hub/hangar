import { useInfiniteQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { api, apiBase } from "@/api/client";
import type { AuditEvent } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { EmptyState, ErrorState, LoadingRows } from "@/components/ui/states";
import { type Column, DataTable } from "@/components/ui/table";
import { formatDateTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { useSearchState } from "@/lib/use-search-state";

type AuditSearch = { actor?: string; action?: string; since?: string; until?: string };

/** Turns a date from the form (YYYY-MM-DD) into the start or end of that day in UTC. */
export function dayBound(date: string | undefined, end: boolean): string | undefined {
  if (!date || !/^\d{4}-\d{2}-\d{2}$/.test(date)) return undefined;
  return `${date}T${end ? "23:59:59" : "00:00:00"}Z`;
}

/** The query string for the audit API, shared by the list and the CSV export. */
export function auditQuery(s: AuditSearch) {
  return {
    ...(s.actor ? { actor: s.actor } : {}),
    ...(s.action ? { action: s.action } : {}),
    ...(dayBound(s.since, false) ? { since: dayBound(s.since, false) } : {}),
    ...(dayBound(s.until, true) ? { until: dayBound(s.until, true) } : {}),
  };
}

/** The audit log with filters and CSV export (EVT-2). */
export function AdminAuditPage() {
  const [search, update] = useSearchState<AuditSearch>();
  const query = auditQuery(search);
  const events = useInfiniteQuery({
    queryKey: ["admin", "audit", query],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/admin/audit", {
          params: { query: { ...query, ...(pageParam ? { cursor: pageParam } : {}) } },
        }),
      ) as { items: AuditEvent[]; next_cursor?: string },
    getNextPageParam: (l) => l.next_cursor || undefined,
  });
  const items = events.data?.pages.flatMap((p) => p.items) ?? [];
  const csv = `${apiBase}/admin/audit?${new URLSearchParams({ ...(query as Record<string, string>), format: "csv" })}`;
  const columns: Column<AuditEvent>[] = [
    {
      key: "time",
      header: "Time",
      cell: (e) => (
        <span className="whitespace-nowrap text-xs">{formatDateTime(e.created_at)}</span>
      ),
    },
    { key: "action", header: "Action", primary: true, cell: (e) => <Badge mono>{e.action}</Badge> },
    {
      key: "actor",
      header: "Actor",
      cell: (e) => <span className="font-mono text-xs">{e.actor ?? "System"}</span>,
    },
    {
      key: "target",
      header: "Target",
      cell: (e) => (
        <span className="break-all font-mono text-xs">
          {[e.target_type, e.target_id].filter(Boolean).join(" ")}
        </span>
      ),
    },
    {
      key: "ip",
      header: "IP address",
      cell: (e) => <span className="font-mono text-xs">{e.ip ?? ""}</span>,
    },
  ];
  return (
    <PageShell
      title="Audit log"
      actions={
        <Button asChild>
          <a href={csv} download="hangar-audit.csv">
            <Download aria-hidden="true" />
            Export CSV
          </a>
        </Button>
      }
    >
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          const get = (k: string) => {
            const v = f.get(k);
            return typeof v === "string" && v ? v : undefined;
          };
          update({
            actor: get("actor"),
            action: get("action"),
            since: get("since"),
            until: get("until"),
          });
        }}
      >
        <Field label="Actor">
          {(p) => <Input {...p} name="actor" className="w-36" defaultValue={search.actor ?? ""} />}
        </Field>
        <Field label="Action">
          {(p) => (
            <Input
              {...p}
              name="action"
              className="w-44 font-mono"
              placeholder="token.created"
              defaultValue={search.action ?? ""}
            />
          )}
        </Field>
        <Field label="From">
          {(p) => (
            <Input
              {...p}
              name="since"
              type="date"
              className="w-40"
              defaultValue={search.since ?? ""}
            />
          )}
        </Field>
        <Field label="To">
          {(p) => (
            <Input
              {...p}
              name="until"
              type="date"
              className="w-40"
              defaultValue={search.until ?? ""}
            />
          )}
        </Field>
        <Button type="submit">Filter</Button>
      </form>
      <p className="text-xs text-muted">Dates are in UTC.</p>
      {events.isPending ? (
        <LoadingRows rows={5} label="Loading audit events" />
      ) : events.isError ? (
        <ErrorState error={events.error} onRetry={() => events.refetch()} />
      ) : items.length === 0 ? (
        <EmptyState title="No audit events match" />
      ) : (
        <DataTable caption="Audit events" columns={columns} rows={items} rowKey={(e) => e.id} />
      )}
      {events.hasNextPage ? (
        <Button onClick={() => events.fetchNextPage()}>Older events</Button>
      ) : null}
    </PageShell>
  );
}
