import { AlertTriangle, Lock, RotateCw } from "lucide-react";
import type { ReactNode } from "react";
import { errorMessage, errorStatus } from "@/lib/problem";
import { cn } from "@/lib/utils";
import { Button } from "./button";

/** A grey placeholder block while data loads. */
export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden="true" className={cn("animate-pulse rounded-md bg-raised", className)} />;
}

/** Placeholder rows for a list that is loading. */
export function LoadingRows({ rows = 4, label = "Loading" }: { rows?: number; label?: string }) {
  return (
    <div role="status" aria-label={label} className="space-y-2">
      {Array.from({ length: rows }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: static placeholders
        <Skeleton key={i} className="h-12 w-full" />
      ))}
      <span className="sr-only">{label}</span>
    </div>
  );
}

/** An empty state: what is missing and what to do next. */
export function EmptyState({
  title,
  children,
  action,
  icon,
  size = "md",
  className,
}: {
  title: string;
  children?: ReactNode;
  action?: ReactNode;
  icon?: ReactNode;
  /** `sm` for empty states inside tabs, drawers and dialogs. */
  size?: "sm" | "md";
  className?: string;
}) {
  return (
    <div
      className={cn(
        // IDs and paths in titles have no spaces, so let them wrap anywhere.
        "hg-enter flex flex-col items-start gap-2 rounded-lg border border-dashed border-line-strong wrap-anywhere",
        size === "sm" ? "px-4 py-5" : "px-5 py-8",
        className,
      )}
    >
      {icon ? <div className="text-muted [&_svg]:size-6">{icon}</div> : null}
      <p className="font-medium">{title}</p>
      {children ? <div className="max-w-prose text-sm text-muted">{children}</div> : null}
      {action ? <div className="mt-2 flex flex-wrap gap-2">{action}</div> : null}
    </div>
  );
}

/** Shown when the API refuses access. */
export function PermissionDenied({ children }: { children?: ReactNode }) {
  return (
    <div
      role="alert"
      className="flex items-start gap-3 rounded-lg border border-line bg-surface px-5 py-6"
    >
      <Lock className="mt-0.5 size-5 shrink-0 text-muted" aria-hidden="true" />
      <div>
        <p className="font-medium">You don't have access to this</p>
        <p className="mt-1 text-sm text-muted">
          {children ??
            "Ask an admin of this Namespace or Instance for access, or sign in with another account."}
        </p>
      </div>
    </div>
  );
}

/**
 * Shows an error from a query: the problem detail and a retry button, or
 * the permission-denied state for 403.
 */
export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  if (errorStatus(error) === 403) return <PermissionDenied />;
  if (errorStatus(error) === 401) {
    return (
      <div
        role="alert"
        className="flex items-start gap-3 rounded-lg border border-line bg-surface px-5 py-6"
      >
        <Lock className="mt-0.5 size-5 shrink-0 text-muted" aria-hidden="true" />
        <div>
          <p className="font-medium">Sign in to see this</p>
          <p className="mt-1 text-sm text-muted">
            This Instance only shows its Arsenal to people who are signed in.
          </p>
          <Button asChild variant="primary" size="sm" className="mt-3">
            <a
              href={`/auth/login?redirect=${encodeURIComponent(window.location.pathname + window.location.search)}`}
            >
              Sign in
            </a>
          </Button>
        </div>
      </div>
    );
  }
  return (
    <div
      role="alert"
      className="hg-enter flex items-start gap-3 rounded-lg border border-danger/30 bg-danger-soft px-5 py-4"
    >
      <AlertTriangle className="mt-0.5 size-5 shrink-0 text-danger" aria-hidden="true" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-danger">This didn't load</p>
        <p className="mt-1 text-sm wrap-anywhere">{errorMessage(error)}</p>
        {onRetry ? (
          <Button size="sm" className="mt-3" onClick={onRetry}>
            <RotateCw aria-hidden="true" />
            Try again
          </Button>
        ) : null}
      </div>
    </div>
  );
}

/**
 * The error under an editor that commits. A 412 means the branch moved
 * after editing started, so the server refused the Commit rather than
 * overwrite the other change: say so, and what to do.
 */
export function CommitError({ error, branch }: { error: unknown; branch: string }) {
  if (!error) return null;
  if (errorStatus(error) === 412) {
    return (
      <p role="alert" className="text-sm text-danger">
        Someone committed to {branch} after you started editing, so Hangar did not save your change.
        Copy your text, reload the page and make your change again.
      </p>
    );
  }
  return <InlineError error={error} />;
}

/** An inline error under a form or action. */
export function InlineError({ error }: { error: unknown }) {
  if (!error) return null;
  return (
    <p role="alert" className="text-sm text-danger">
      {errorMessage(error)}
    </p>
  );
}
