import { type ReactNode, useState } from "react";
import { errorMessage } from "@/lib/problem";
import { Button } from "./button";
import { Dialog } from "./dialog";
import { Input } from "./form";

/**
 * Asks before a destructive action. With `confirmText`, the user types it
 * to enable the button, as for deleting a Repo.
 */
export function ConfirmDialog({
  trigger,
  title,
  children,
  actionLabel,
  onConfirm,
  confirmText,
  danger = true,
}: {
  trigger: ReactNode;
  title: string;
  children: ReactNode;
  actionLabel: string;
  onConfirm: () => Promise<unknown>;
  confirmText?: string;
  danger?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        setTyped("");
        setError(null);
      }}
      trigger={trigger}
      title={title}
      footer={
        <>
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant={danger ? "danger" : "primary"}
            disabled={busy || (confirmText !== undefined && typed !== confirmText)}
            onClick={async () => {
              setBusy(true);
              setError(null);
              try {
                await onConfirm();
                setOpen(false);
              } catch (e) {
                setError(e);
              } finally {
                setBusy(false);
              }
            }}
          >
            {actionLabel}
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-sm">
        {children}
        {confirmText !== undefined ? (
          <label className="block space-y-1.5">
            <span>
              Type <code className="font-mono">{confirmText}</code> to confirm.
            </span>
            <Input value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
          </label>
        ) : null}
        {error ? (
          <p role="alert" className="text-danger">
            {errorMessage(error)}
          </p>
        ) : null}
      </div>
    </Dialog>
  );
}
