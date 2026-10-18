import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api, rawFetch } from "@/api/client";
import type { Me } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { InlineError } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { ApiError, fieldErrors, unwrap, unwrapEmpty } from "@/lib/problem";
import { meKey, useMe } from "@/lib/session";

/**
 * The user's account: username and role, their display name and email,
 * their password, and linked sign-in providers (W8 8, 05-auth.md).
 */
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
        <dt className="text-muted">Role</dt>
        <dd>{u.is_admin ? <Badge tone="accent">Instance admin</Badge> : "User"}</dd>
      </dl>
      <ProfileForm me={u} />
      <PasswordForm me={u} />
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
    </PageShell>
  );
}

/** Display name and email, saved with PATCH /me. */
function ProfileForm({ me }: { me: Me }) {
  const qc = useQueryClient();
  const toast = useToast();
  // The server shows the username when no display name is set.
  const savedName = me.display_name === me.username ? "" : me.display_name;
  const savedEmail = me.email ?? "";
  const [name, setName] = useState(savedName);
  const [email, setEmail] = useState(savedEmail);
  useEffect(() => {
    setName(savedName);
    setEmail(savedEmail);
  }, [savedName, savedEmail]);
  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.PATCH("/me", {
          body: { display_name: name.trim(), email: email.trim() },
        }),
      ),
    onSuccess: (updated) => {
      qc.setQueryData(meKey, updated);
      toast("Profile saved.");
    },
  });
  const errs = fieldErrors(save.error);
  const changed = name.trim() !== savedName || email.trim() !== savedEmail;
  return (
    <form
      aria-labelledby="profile-details"
      className="max-w-xl space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2 id="profile-details" className="font-semibold">
        Name and email
      </h2>
      <Field
        label="Display name"
        hint="Shown instead of your username. Leave it empty to show your username."
        error={errs.display_name}
      >
        {(p) => (
          <Input
            {...p}
            autoComplete="name"
            maxLength={100}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        )}
      </Field>
      <Field label="Email" hint="Optional. Admins use it to recognise you." error={errs.email}>
        {(p) => (
          <Input
            {...p}
            type="email"
            autoComplete="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        )}
      </Field>
      {save.error && !errs.display_name && !errs.email ? <InlineError error={save.error} /> : null}
      <Button type="submit" variant="primary" disabled={!changed || save.isPending}>
        Save profile
      </Button>
    </form>
  );
}

/** Changes the password with POST /me/password, which needs this session. */
function PasswordForm({ me }: { me: Me }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [mismatch, setMismatch] = useState(false);
  const change = useMutation({
    mutationFn: async () =>
      unwrapEmpty(
        await api.POST("/me/password", {
          body: { current_password: current, new_password: next },
        }),
      ),
    onSuccess: () => {
      setCurrent("");
      setNext("");
      setConfirm("");
    },
  });
  if (!me.has_password) {
    return (
      <section aria-labelledby="password" className="max-w-xl space-y-2">
        <h2 id="password" className="font-semibold">
          Password
        </h2>
        <p className="text-sm text-muted">
          You sign in with a sign-in provider, so your account has no Hangar password. Change your
          password with your provider.
        </p>
      </section>
    );
  }
  const errs = fieldErrors(change.error);
  return (
    <form
      aria-labelledby="password"
      className="max-w-xl space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (next !== confirm) {
          setMismatch(true);
          return;
        }
        setMismatch(false);
        change.mutate();
      }}
    >
      <h2 id="password" className="font-semibold">
        Password
      </h2>
      {change.isSuccess ? (
        <p
          role="status"
          className="rounded-md border border-ok/30 bg-ok-soft px-3 py-2 text-sm text-ok"
        >
          Password changed. Your other sessions are signed out; this one stays signed in. Tokens
          keep working.
        </p>
      ) : null}
      <Field label="Current password" error={errs.current_password}>
        {(p) => (
          <Input
            {...p}
            type="password"
            required
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        )}
      </Field>
      <Field
        label="New password"
        hint="At least 10 characters, and not a common password."
        error={errs.new_password}
      >
        {(p) => (
          <Input
            {...p}
            type="password"
            required
            autoComplete="new-password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
        )}
      </Field>
      <Field
        label="Confirm new password"
        error={mismatch ? "The new passwords do not match." : undefined}
      >
        {(p) => (
          <Input
            {...p}
            type="password"
            required
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
        )}
      </Field>
      {change.error && !errs.current_password && !errs.new_password ? (
        <InlineError error={change.error} />
      ) : null}
      <Button type="submit" variant="primary" disabled={change.isPending}>
        Change password
      </Button>
    </form>
  );
}
