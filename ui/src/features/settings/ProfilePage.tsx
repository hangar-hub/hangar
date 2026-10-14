import { useMutation, useQuery } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { api, rawFetch } from "@/api/client";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { InlineError } from "@/components/ui/states";
import { ApiError, unwrap } from "@/lib/problem";
import { useMe } from "@/lib/session";

/** The user's account: names, email, admin status and linked sign-in providers. */
export function ProfilePage() {
  const me = useMe();
  const search = useSearch({ strict: false }) as { linked?: string };
  const providers = useQuery({
    queryKey: ["auth", "providers"],
    queryFn: async () => unwrap(await api.GET("/auth/providers")),
  });
  const link = useMutation({
    mutationFn: async (provider: string) => {
      const res = await rawFetch(`/auth/oidc/${encodeURIComponent(provider)}/link`, {
        method: "POST",
      });
      const body = (await res.json().catch(() => undefined)) as
        | { authorize_url?: string }
        | undefined;
      if (!res.ok || !body?.authorize_url)
        throw new ApiError(res.status, undefined, "Linking could not start. Try again.");
      window.location.assign(body.authorize_url);
    },
  });
  const u = me.data;
  if (!u) return null;
  return (
    <PageShell title="Profile" subtitle="Your account on this Instance.">
      {search.linked ? (
        <p
          role="status"
          className="rounded-md border border-ok/30 bg-ok-soft px-3 py-2 text-sm text-ok"
        >
          Linked your {search.linked} account. You can sign in with it now.
        </p>
      ) : null}
      <dl className="grid max-w-xl gap-x-6 gap-y-2 text-sm sm:grid-cols-[max-content_1fr]">
        <dt className="text-muted">Username</dt>
        <dd className="font-mono">{u.username}</dd>
        <dt className="text-muted">Display name</dt>
        <dd>{u.display_name || "Not set"}</dd>
        <dt className="text-muted">Email</dt>
        <dd>{u.email || "Not set"}</dd>
        <dt className="text-muted">Role</dt>
        <dd>{u.is_admin ? <Badge tone="accent">Instance admin</Badge> : "User"}</dd>
      </dl>
      {providers.data?.providers.length ? (
        <section aria-labelledby="linked" className="space-y-2">
          <h2 id="linked" className="font-semibold">
            Sign-in providers
          </h2>
          <p className="text-sm text-muted">Link a provider to sign in with it as this account.</p>
          <div className="flex flex-wrap gap-2">
            {providers.data.providers.map((p) => (
              <Button key={p.id} onClick={() => link.mutate(p.id)} disabled={link.isPending}>
                Link {p.name}
              </Button>
            ))}
          </div>
          <InlineError error={link.error} />
        </section>
      ) : null}
      <p className="text-sm text-muted">To change your password, ask an admin for a reset link.</p>
    </PageShell>
  );
}
