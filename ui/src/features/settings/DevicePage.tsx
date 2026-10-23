import { useMutation } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api/client";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/form";
import { InlineError } from "@/components/ui/states";
import { unwrapEmpty } from "@/lib/problem";

/** Formats what the user types as a device code: upper case, XXXX-XXXX. */
export function normaliseUserCode(s: string): string {
  const raw = s
    .toUpperCase()
    .replace(/[^A-Z0-9]/g, "")
    .slice(0, 8);
  return raw.length > 4 ? `${raw.slice(0, 4)}-${raw.slice(4)}` : raw;
}

/** Approves or denies a device-code login from `hf auth login` (05-auth.md). */
export function DevicePage() {
  const search = useSearch({ strict: false }) as { code?: string };
  const [code, setCode] = useState(normaliseUserCode(search.code ?? ""));
  const decide = useMutation({
    mutationFn: async (approve: boolean) => {
      unwrapEmpty(await api.POST("/me/device", { body: { user_code: code, approve } }));
      return approve;
    },
  });
  return (
    <PageShell
      title="Connect a device"
      subtitle="Enter the code your terminal shows to sign it in as you."
    >
      {decide.isSuccess ? (
        <p
          role="status"
          className="hg-enter max-w-sm rounded-md border border-ok/30 bg-ok-soft px-3 py-2 text-sm text-ok"
        >
          {decide.data
            ? "Approved. Go back to your terminal: it signs in within a few seconds."
            : "Denied. The terminal is not signed in."}
        </p>
      ) : (
        <form
          className="max-w-sm space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            decide.mutate(true);
          }}
        >
          <Field label="Code">
            {(p) => (
              <Input
                {...p}
                required
                className="font-mono text-lg tracking-widest"
                value={code}
                onChange={(e) => setCode(normaliseUserCode(e.target.value))}
                placeholder="ABCD-1234"
              />
            )}
          </Field>
          <InlineError error={decide.error} />
          <div className="flex gap-2">
            <Button
              type="submit"
              variant="primary"
              disabled={code.length !== 9 || decide.isPending}
            >
              Approve
            </Button>
            <Button
              onClick={() => decide.mutate(false)}
              disabled={code.length !== 9 || decide.isPending}
            >
              Deny
            </Button>
          </div>
        </form>
      )}
    </PageShell>
  );
}
