import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/api/client";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { ErrorState, LoadingRows } from "@/components/ui/states";
import { formatDate, relativeTime } from "@/lib/format";
import { unwrap } from "@/lib/problem";

/** Where you're signed in, with a way to end other sessions. */
export function SessionsPage() {
  const qc = useQueryClient();
  const sessions = useQuery({
    queryKey: ["me", "sessions"],
    queryFn: async () => unwrap(await api.GET("/me/sessions")),
  });
  return (
    <PageShell title="Sessions" subtitle="Browsers where you're signed in.">
      {sessions.isPending ? (
        <LoadingRows rows={2} label="Loading sessions" />
      ) : sessions.isError ? (
        <ErrorState error={sessions.error} onRetry={() => sessions.refetch()} />
      ) : (
        <ul className="hg-enter divide-y divide-line rounded-lg border border-line bg-surface">
          {sessions.data.items.map((s) => (
            <li
              key={s.id}
              className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-3 text-sm"
            >
              <span className="min-w-0 flex-1 truncate" title={s.user_agent || undefined}>
                {s.user_agent || "Unknown browser"}
              </span>
              {s.current ? <Badge tone="ok">This browser</Badge> : null}
              <span className="tabular font-mono text-xs text-muted">{s.ip}</span>
              <span className="tabular text-xs text-muted">
                Active {relativeTime(s.last_seen_at ?? s.created_at)}, expires{" "}
                {formatDate(s.expires_at)}
              </span>
              {!s.current ? (
                <ConfirmDialog
                  trigger={
                    <Button size="sm" variant="danger">
                      Sign out
                    </Button>
                  }
                  title="Sign out this session?"
                  actionLabel="Sign out"
                  onConfirm={async () => {
                    const res = await api.DELETE("/me/sessions/{id}", {
                      params: { path: { id: s.id } },
                    });
                    if (!res.response.ok) unwrap(res);
                    qc.invalidateQueries({ queryKey: ["me", "sessions"] });
                  }}
                >
                  <p>That browser has to sign in again.</p>
                </ConfirmDialog>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </PageShell>
  );
}
