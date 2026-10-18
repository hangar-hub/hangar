import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { KeyRound } from "lucide-react";
import { useState } from "react";
import { api, rawFetch } from "@/api/client";
import type { Me } from "@/api/types";
import { useGo } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { ErrorState, InlineError, Skeleton } from "@/components/ui/states";
import { ApiError, fieldErrors, unwrap } from "@/lib/problem";
import { useSignedIn } from "@/lib/session";

/** Messages for the `?error=` codes the OIDC callback redirects with (W5's `/auth/` handler). */
export const oidcErrors: Record<string, string> = {
  oidc_state: "The sign-in took too long or was started in another tab. Try again.",
  oidc_link_required:
    "An account with this email already exists. Sign in with your password, then link the provider from Settings.",
  oidc_no_account:
    "There is no Hangar account for this identity, and this Instance does not create accounts automatically. Ask an admin for an invite.",
  oidc_identity_taken: "This identity is linked to another Hangar account.",
  account_disabled: "This account is disabled. Ask an admin to enable it.",
  oidc_unknown_provider: "That sign-in provider is not configured on this Instance.",
  oidc_denied: "The provider did not let you sign in.",
  oidc_failed:
    "Signing in with the provider failed. Try again, and ask an admin to check the logs if it keeps happening.",
};

/**
 * Only same-origin paths are followed after sign-in, never another site,
 * with the rules of internal/auth.SafeRedirect: browsers read "/\\" as "//".
 */
export function safeRedirect(target: unknown): string {
  return typeof target === "string" &&
    target.startsWith("/") &&
    !target.startsWith("//") &&
    !target.startsWith("/\\") &&
    // biome-ignore lint/suspicious/noControlCharactersInRegex: control characters are what it refuses.
    !/[\u0000-\u001f\u007f]/.test(target) &&
    target.length <= 2000
    ? target
    : "/";
}

/** Sign-in: the password form and one button per OIDC provider; registration when open. */
export function LoginPage() {
  const search = useSearch({ strict: false }) as {
    redirect?: string;
    error?: string;
    register?: string;
  };
  const redirect = safeRedirect(search.redirect);
  const [register, setRegister] = useState(search.register === "1");
  const [form, setForm] = useState({ username: "", password: "", email: "", display_name: "" });
  const signedIn = useSignedIn();
  const go = useGo();
  const navigate = useNavigate();
  const providers = useQuery({
    queryKey: ["auth", "providers"],
    queryFn: async () => unwrap(await api.GET("/auth/providers")),
  });
  const done = (me: Me) => {
    signedIn(me);
    go(redirect);
  };
  const login = useMutation({
    mutationFn: async () => {
      if (register) {
        const res = await rawFetch("/auth/register", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(form),
        });
        const body = await res.json().catch(() => undefined);
        if (!res.ok) throw new ApiError(res.status, body);
        return body as Me;
      }
      return unwrap(
        await api.POST("/auth/login", {
          body: { username: form.username, password: form.password },
        }),
      );
    },
    onSuccess: done,
  });
  const errs = fieldErrors(login.error);
  if (providers.isPending) return <Skeleton className="mx-auto h-80 max-w-sm" />;
  if (providers.isError)
    return <ErrorState error={providers.error} onRetry={() => providers.refetch()} />;
  const p = providers.data;
  const canRegister = p.registration === "open";
  return (
    <section aria-labelledby="page-title" className="mx-auto max-w-sm py-8">
      <h1 id="page-title" className="text-2xl font-semibold tracking-tight">
        {register ? "Create an account" : "Sign in to Hangar"}
      </h1>
      {search.error ? (
        <p
          role="alert"
          className="mt-4 rounded-md border border-danger/30 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {oidcErrors[search.error] ?? oidcErrors.oidc_failed}
        </p>
      ) : null}
      {p.providers.length ? (
        <div className="mt-6 space-y-2">
          {p.providers.map((prov) => (
            <Button key={prov.id} asChild className="w-full" size="lg">
              <a
                href={`${prov.login_url}${prov.login_url.includes("?") ? "&" : "?"}redirect=${encodeURIComponent(redirect)}`}
              >
                <KeyRound aria-hidden="true" />
                Sign in with {prov.name}
              </a>
            </Button>
          ))}
        </div>
      ) : null}
      {p.local_login || register ? (
        <>
          {p.providers.length ? (
            <p className="my-5 text-center text-xs text-muted">or use a username and password</p>
          ) : null}
          <form
            className="mt-6 space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              login.mutate();
            }}
          >
            <Field label="Username" error={errs.username}>
              {(fp) => (
                <Input
                  {...fp}
                  autoComplete="username"
                  required
                  value={form.username}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                />
              )}
            </Field>
            {register ? (
              <>
                <Field
                  label="Email"
                  hint="Optional. It takes effect when you open the confirmation link an admin sends to it."
                  error={errs.email}
                >
                  {(fp) => (
                    <Input
                      {...fp}
                      type="email"
                      autoComplete="email"
                      value={form.email}
                      onChange={(e) => setForm({ ...form, email: e.target.value })}
                    />
                  )}
                </Field>
                <Field label="Display name" error={errs.display_name}>
                  {(fp) => (
                    <Input
                      {...fp}
                      autoComplete="name"
                      value={form.display_name}
                      onChange={(e) => setForm({ ...form, display_name: e.target.value })}
                    />
                  )}
                </Field>
              </>
            ) : null}
            <Field
              label="Password"
              hint={register ? "At least 10 characters." : undefined}
              error={errs.password}
            >
              {(fp) => (
                <Input
                  {...fp}
                  type="password"
                  autoComplete={register ? "new-password" : "current-password"}
                  required
                  value={form.password}
                  onChange={(e) => setForm({ ...form, password: e.target.value })}
                />
              )}
            </Field>
            {login.error && !Object.keys(errs).length ? <InlineError error={login.error} /> : null}
            <Button
              type="submit"
              variant="primary"
              className="w-full"
              size="lg"
              disabled={login.isPending}
            >
              {register ? "Create account" : "Sign in"}
            </Button>
          </form>
        </>
      ) : (
        <p className="mt-6 text-sm text-muted">
          This Instance signs people in through the providers above only.
        </p>
      )}
      {canRegister ? (
        <p className="mt-6 text-sm text-muted">
          {register ? "Already have an account? " : "New to this Instance? "}
          <button
            type="button"
            className="text-accent underline"
            onClick={() => {
              setRegister(!register);
              navigate({
                to: "/auth/login",
                search: register ? {} : { register: "1" },
                replace: true,
              });
            }}
          >
            {register ? "Sign in" : "Create an account"}
          </button>
        </p>
      ) : !register ? (
        <p className="mt-6 text-sm text-muted">
          Forgotten your password? Ask an admin for a reset link.
        </p>
      ) : null}
    </section>
  );
}
