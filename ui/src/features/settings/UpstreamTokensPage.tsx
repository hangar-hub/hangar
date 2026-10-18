import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api/client";
import type { Upstream } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDate } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { useUpstreams } from "../repos/useInstance";

function UpstreamToken({ u }: { u: Upstream }) {
  const qc = useQueryClient();
  const toast = useToast();
  const key = ["me", "upstream-tokens", u.name];
  const status = useQuery({
    queryKey: key,
    queryFn: async () =>
      unwrap(
        await api.GET("/me/upstream-tokens/{upstream}", { params: { path: { upstream: u.name } } }),
      ),
  });
  const [token, setToken] = useState("");
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.PUT("/me/upstream-tokens/{upstream}", {
          params: { path: { upstream: u.name } },
          body: { token },
        }),
      ),
    onSuccess: (s) => {
      setToken("");
      qc.setQueryData(key, s);
      toast(`Saved your token for ${u.display_name}.`);
    },
  });
  const remove = useMutation({
    mutationFn: async () => {
      const res = await api.DELETE("/me/upstream-tokens/{upstream}", {
        params: { path: { upstream: u.name } },
      });
      if (!res.response.ok) unwrap(res);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: key });
      toast(`Removed your token for ${u.display_name}.`);
    },
  });
  return (
    <li className="space-y-3 rounded-lg border border-line bg-surface p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="font-semibold">{u.display_name}</h2>
        <span className="min-w-0 font-mono text-xs text-muted wrap-anywhere">{u.base_url}</span>
        {status.data?.saved ? (
          <Badge tone="ok">
            Token saved{status.data.updated_at ? ` ${formatDate(status.data.updated_at)}` : ""}
          </Badge>
        ) : (
          <Badge>No token</Badge>
        )}
      </div>
      {status.isError ? <ErrorState error={status.error} onRetry={() => status.refetch()} /> : null}
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <Field
          label={status.data?.saved ? "Replace the token" : "Token"}
          className="min-w-60 flex-1"
        >
          {(p) => (
            <Input
              {...p}
              type="password"
              autoComplete="off"
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          )}
        </Field>
        <Button type="submit" variant="primary" disabled={!token || save.isPending}>
          Save token
        </Button>
        {status.data?.saved ? (
          <Button variant="danger" onClick={() => remove.mutate()} disabled={remove.isPending}>
            Remove
          </Button>
        ) : null}
      </form>
      <InlineError error={save.error ?? remove.error} />
    </li>
  );
}

/** Your own tokens for Upstreams, used for gated and private Pulls (PULL-8). Hangar never shows them again. */
export function UpstreamTokensPage() {
  const upstreams = useUpstreams();
  return (
    <PageShell
      title="Upstream tokens"
      subtitle="Pulls you start use your token for that Upstream, so gated and private Repos you can access there work here too."
    >
      {upstreams.isPending ? (
        <LoadingRows rows={2} label="Loading Upstreams" />
      ) : upstreams.isError ? (
        <ErrorState error={upstreams.error} onRetry={() => upstreams.refetch()} />
      ) : upstreams.data.items.length === 0 ? (
        <EmptyState title="No Upstreams are enabled" />
      ) : (
        <ul className="hg-enter space-y-3">
          {upstreams.data.items.map((u) => (
            <UpstreamToken key={u.name} u={u} />
          ))}
        </ul>
      )}
    </PageShell>
  );
}
