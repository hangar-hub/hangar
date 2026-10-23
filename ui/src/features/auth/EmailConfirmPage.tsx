import { useMutation, useQueryClient } from "@tanstack/react-query";
import { rawFetch } from "@/api/client";
import { AppLink } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { InlineError } from "@/components/ui/states";
import { useDocumentTitle } from "@/lib/page";
import { ApiError } from "@/lib/problem";
import { meKey } from "@/lib/session";

/**
 * Confirms a pending email from the single-use link an admin sends to that
 * address (05-auth.md, "Local accounts"). It asks before it confirms, so a
 * mail scanner that opens the link changes nothing.
 */
export function EmailConfirmPage({ code }: { code: string }) {
  useDocumentTitle("Confirm your email");
  const qc = useQueryClient();
  const confirm = useMutation({
    mutationFn: async () => {
      const res = await rawFetch(`/auth/email/${encodeURIComponent(code)}`, { method: "POST" });
      if (!res.ok) {
        throw new ApiError(res.status, await res.json().catch(() => undefined));
      }
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: meKey }),
  });
  return (
    <section aria-labelledby="page-title" className="mx-auto max-w-sm py-8">
      <h1 id="page-title" className="text-2xl font-semibold tracking-tight">
        Confirm your email
      </h1>
      {confirm.isSuccess ? (
        <div className="hg-enter mt-4 space-y-3">
          <p
            role="status"
            className="rounded-md border border-ok/30 bg-ok-soft px-3 py-2 text-sm text-ok"
          >
            Email confirmed. It is now the email of your Hangar account.
          </p>
          <AppLink href="/settings/profile" className="hg-link text-sm">
            Go to your profile settings
          </AppLink>
        </div>
      ) : (
        <div className="mt-2 space-y-4">
          <p className="text-sm text-muted">
            Confirm that this email address is yours, to make it the email of your Hangar account.
            The link works once.
          </p>
          <InlineError error={confirm.error} />
          <Button variant="primary" onClick={() => confirm.mutate()} disabled={confirm.isPending}>
            Confirm email
          </Button>
        </div>
      )}
    </section>
  );
}
