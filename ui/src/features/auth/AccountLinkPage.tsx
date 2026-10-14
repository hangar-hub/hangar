import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { rawFetch } from "@/api/client";
import type { Me } from "@/api/types";
import { useGo } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { InlineError } from "@/components/ui/states";
import { ApiError, fieldErrors } from "@/lib/problem";
import { useSignedIn } from "@/lib/session";

/**
 * Invite acceptance and password reset, from the single-use links admins
 * copy (AUTH-1). Both post to the `/auth/` handler and sign the user in.
 */
export function AccountLinkPage({ mode, code }: { mode: "invite" | "reset"; code: string }) {
  const [form, setForm] = useState({
    username: "",
    password: "",
    confirm: "",
    email: "",
    display_name: "",
  });
  const [mismatch, setMismatch] = useState(false);
  const signedIn = useSignedIn();
  const go = useGo();
  const submit = useMutation({
    mutationFn: async () => {
      const body =
        mode === "reset"
          ? { password: form.password }
          : {
              username: form.username,
              password: form.password,
              email: form.email,
              display_name: form.display_name,
            };
      const res = await rawFetch(`/auth/${mode}/${encodeURIComponent(code)}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      const json = await res.json().catch(() => undefined);
      if (!res.ok) throw new ApiError(res.status, json);
      return json as Me;
    },
    onSuccess: (me) => {
      signedIn(me);
      go("/");
    },
  });
  const errs = fieldErrors(submit.error);
  return (
    <section aria-labelledby="page-title" className="mx-auto max-w-sm py-8">
      <h1 id="page-title" className="text-2xl font-semibold tracking-tight">
        {mode === "invite" ? "Accept your invite" : "Choose a new password"}
      </h1>
      <p className="mt-2 text-sm text-muted">
        {mode === "invite"
          ? "Create your Hangar account. The link works once."
          : "Your other sessions end when you save the new password."}
      </p>
      <form
        className="mt-6 space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (form.password !== form.confirm) {
            setMismatch(true);
            return;
          }
          setMismatch(false);
          submit.mutate();
        }}
      >
        {mode === "invite" ? (
          <>
            <Field
              label="Username"
              hint="Also your personal Namespace, as in username/model-name."
              error={errs.username}
            >
              {(p) => (
                <Input
                  {...p}
                  required
                  autoComplete="username"
                  value={form.username}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                />
              )}
            </Field>
            <Field label="Email" error={errs.email}>
              {(p) => (
                <Input
                  {...p}
                  type="email"
                  autoComplete="email"
                  value={form.email}
                  onChange={(e) => setForm({ ...form, email: e.target.value })}
                />
              )}
            </Field>
            <Field label="Display name" error={errs.display_name}>
              {(p) => (
                <Input
                  {...p}
                  autoComplete="name"
                  value={form.display_name}
                  onChange={(e) => setForm({ ...form, display_name: e.target.value })}
                />
              )}
            </Field>
          </>
        ) : null}
        <Field label="Password" hint="At least 10 characters." error={errs.password}>
          {(p) => (
            <Input
              {...p}
              type="password"
              required
              autoComplete="new-password"
              value={form.password}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
            />
          )}
        </Field>
        <Field label="Confirm password" error={mismatch ? "The passwords don't match." : undefined}>
          {(p) => (
            <Input
              {...p}
              type="password"
              required
              autoComplete="new-password"
              value={form.confirm}
              onChange={(e) => setForm({ ...form, confirm: e.target.value })}
            />
          )}
        </Field>
        {submit.error && !Object.keys(errs).length ? <InlineError error={submit.error} /> : null}
        <Button
          type="submit"
          variant="primary"
          size="lg"
          className="w-full"
          disabled={submit.isPending}
        >
          {mode === "invite" ? "Create account" : "Save password"}
        </Button>
      </form>
    </section>
  );
}
