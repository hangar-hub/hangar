import { X } from "lucide-react";
import { Dialog as RadixDialog } from "radix-ui";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/**
 * A modal dialog. `side` turns it into a sheet that slides in from the
 * right (drawers, filters on phones).
 */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  side,
  wide,
  trigger,
}: {
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  side?: "right" | "left";
  wide?: boolean;
  trigger?: ReactNode;
}) {
  return (
    <RadixDialog.Root open={open} onOpenChange={onOpenChange}>
      {trigger ? <RadixDialog.Trigger asChild>{trigger}</RadixDialog.Trigger> : null}
      <RadixDialog.Portal>
        <RadixDialog.Overlay className="fixed inset-0 z-40 bg-steel-950/50" />
        <RadixDialog.Content
          className={cn(
            "fixed z-50 flex flex-col border border-line bg-surface text-ink shadow-xl focus:outline-none",
            side
              ? cn(
                  "inset-y-0 w-full max-w-md",
                  side === "right" ? "right-0 border-l" : "left-0 border-r",
                  wide && "max-w-2xl",
                )
              : cn(
                  "left-1/2 top-1/2 max-h-[90vh] w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 rounded-lg",
                  wide ? "max-w-3xl" : "max-w-lg",
                ),
          )}
        >
          <div className="flex items-start justify-between gap-4 border-b border-line px-5 py-3.5">
            <div>
              <RadixDialog.Title className="text-base font-semibold">{title}</RadixDialog.Title>
              {description ? (
                <RadixDialog.Description className="mt-0.5 text-sm text-muted">
                  {description}
                </RadixDialog.Description>
              ) : (
                <RadixDialog.Description className="sr-only">{title}</RadixDialog.Description>
              )}
            </div>
            <RadixDialog.Close
              className="-mr-1 rounded-md p-1 text-muted hover:bg-raised hover:text-ink"
              aria-label="Close"
            >
              <X className="size-4" />
            </RadixDialog.Close>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
          {footer ? (
            <div className="flex flex-wrap justify-end gap-2 border-t border-line px-5 py-3">
              {footer}
            </div>
          ) : null}
        </RadixDialog.Content>
      </RadixDialog.Portal>
    </RadixDialog.Root>
  );
}

export const DialogClose = RadixDialog.Close;
