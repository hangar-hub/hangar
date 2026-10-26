import { useMutation } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api/client";
import type { DeviceRequest } from "@/api/types";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { InlineError } from "@/components/ui/states";
import { formatDateTime, relativeTime } from "@/lib/format";
import { unwrap, unwrapEmpty } from "@/lib/problem";
import { scopeHelp } from "./TokensPage";

type DeviceScope = "read" | "write";

/** Formats what the user types as a device code: upper case, XXXX-XXXX. */
export function normaliseUserCode(s: string): string {
  const raw = s
    .toUpperCase()
    .replace(/[^A-Z0-9]/g, "")
    .slice(0, 8);
  return raw.length > 4 ? `${raw.slice(0, 4)}-${raw.slice(4)}` : raw;
}

/**
 * Approves or denies a device-code login from `hf auth login` or
 * `hangar login` (05-auth.md "Device login"). The code is never taken from
 * the URL: the user types the code their own terminal shows, then checks
 * the client, time and IP address before approving, so a link someone
 * else sends cannot sign their device in as this user with one click.
 */
export function DevicePage() {
  const search = useSearch({ strict: false }) as { code?: string };
  const fromLink = Boolean(search.code);
  const [code, setCode] = useState("");
  const [scope, setScope] = useState<DeviceScope>("read");
  const lookup = useMutation({
    mutationFn: async (userCode: string) =>
      unwrap(await api.POST("/me/device/lookup", { body: { user_code: userCode } })),
  });
  const decide = useMutation({
    mutationFn: async (vars: { userCode: string; approve: boolean }) => {
      unwrapEmpty(
        await api.POST("/me/device", {
          body: { user_code: vars.userCode, approve: vars.approve, scope },
        }),
      );
      return vars.approve;
    },
  });
  const request = lookup.data;
  return (
    <PageShell
      title="Connect a device"
      subtitle="Type the code your terminal shows to sign it in as you."
    >
      {decide.isSuccess ? (
        <p
          role="status"
          className="hg-enter max-w-md rounded-md border border-ok/30 bg-ok-soft px-3 py-2 text-sm text-ok"
        >
          {decide.data
            ? "Approved. Go back to your terminal: it signs in within a few seconds."
            : "Denied. The terminal is not signed in."}
        </p>
      ) : request ? (
        <DeviceDetails
          request={request}
          scope={scope}
          onScope={setScope}
          pending={decide.isPending}
          error={decide.error}
          onDecide={(approve) => decide.mutate({ userCode: request.user_code, approve })}
          onBack={() => {
            lookup.reset();
            decide.reset();
            setCode("");
          }}
        />
      ) : (
        <form
          className="max-w-md space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            lookup.mutate(code);
          }}
        >
          {fromLink && (
            <p
              role="note"
              className="rounded-md border border-warn/30 bg-warn-soft px-3 py-2 text-sm text-warn"
            >
              Hangar does not fill in a code from a link. Type the code your own terminal shows. If
              you did not run <code>hf auth login</code> or <code>hangar login</code> just now,
              close this page.
            </p>
          )}
          <Field label="Code">
            {(p) => (
              <Input
                {...p}
                required
                autoComplete="off"
                className="font-mono text-lg tracking-widest"
                value={code}
                onChange={(e) => setCode(normaliseUserCode(e.target.value))}
                placeholder="ABCD-1234"
              />
            )}
          </Field>
          <InlineError error={lookup.error} />
          <Button type="submit" variant="primary" disabled={code.length !== 9 || lookup.isPending}>
            Continue
          </Button>
        </form>
      )}
    </PageShell>
  );
}

function DeviceDetails({
  request,
  scope,
  onScope,
  pending,
  error,
  onDecide,
  onBack,
}: {
  request: DeviceRequest;
  scope: DeviceScope;
  onScope: (s: DeviceScope) => void;
  pending: boolean;
  error: unknown;
  onDecide: (approve: boolean) => void;
  onBack: () => void;
}) {
  return (
    <div className="max-w-md space-y-4">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 rounded-md border border-line px-3 py-2 text-sm">
        <dt className="text-muted">Code</dt>
        <dd className="font-mono">{request.user_code}</dd>
        <dt className="text-muted">Client</dt>
        <dd className="font-mono break-all">{request.client_id || "Unknown"}</dd>
        <dt className="text-muted">Requested</dt>
        <dd>
          {relativeTime(request.requested_at)} ({formatDateTime(request.requested_at)})
        </dd>
        <dt className="text-muted">From IP address</dt>
        <dd className="font-mono break-all">{request.client_ip || "Unknown"}</dd>
      </dl>
      <p
        role="alert"
        className="rounded-md border border-warn/30 bg-warn-soft px-3 py-2 text-sm text-warn"
      >
        Approve only if you started this login yourself, on a device you trust, and the details
        above match. Approving creates a Token that acts as you, with the access you choose below,
        on everything your account can reach. It expires in {request.token_lifetime_days} days. You
        can revoke it at any time in Settings &gt; Tokens.
      </p>
      <fieldset className="space-y-1.5">
        <legend className="text-sm font-medium">Access</legend>
        {(["read", "write"] as DeviceScope[]).map((s) => (
          <label
            key={s}
            className="flex cursor-pointer items-start gap-2 rounded-md border border-line px-3 py-2 text-sm transition-[border-color,background-color] duration-150 hover:border-line-strong has-[:checked]:border-accent/50 has-[:checked]:bg-accent-soft/50"
          >
            <input
              type="radio"
              name="scope"
              className="mt-0.5 size-4 shrink-0 accent-[var(--hg-accent-fill)]"
              checked={scope === s}
              onChange={() => onScope(s)}
            />
            <span>
              <span className="font-mono">{s}</span>
              <span className="block text-xs text-muted">{scopeHelp[s]}</span>
            </span>
          </label>
        ))}
      </fieldset>
      <InlineError error={error} />
      <div className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={() => onDecide(true)} disabled={pending}>
          Approve with {scope} access
        </Button>
        <Button onClick={() => onDecide(false)} disabled={pending}>
          Deny
        </Button>
        <Button variant="ghost" onClick={onBack} disabled={pending}>
          Use another code
        </Button>
      </div>
    </div>
  );
}
