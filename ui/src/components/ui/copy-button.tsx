import { Check, Copy, X } from "lucide-react";
import { type ReactNode, type RefObject, useRef, useState } from "react";
import { copyText, selectText } from "@/lib/clipboard";
import { cn } from "@/lib/utils";
import { Button } from "./button";
import { useToast } from "./toast";

type CopyState = "idle" | "copied" | "failed";

const failed = "Couldn't copy to the clipboard. Select the text and copy it instead.";
const failedSelected =
  "Couldn't copy to the clipboard. The text is selected: press Ctrl+C, or Command+C on a Mac, to copy it.";

/**
 * Copies text to the clipboard and confirms it in place. When the browser
 * does not allow copying, it says so, and selects `selectOnFail`'s text
 * when given, so the user can copy it with the keyboard.
 */
export function CopyButton({
  value,
  label = "Copy",
  showLabel,
  size = "icon-sm",
  selectOnFail,
}: {
  value: string;
  label?: string;
  showLabel?: boolean;
  size?: "icon-sm" | "sm";
  selectOnFail?: RefObject<HTMLElement | null>;
}) {
  const [state, setState] = useState<CopyState>("idle");
  const toast = useToast();
  return (
    <Button
      variant="ghost"
      size={showLabel ? "sm" : size}
      aria-label={showLabel ? undefined : label}
      title={label}
      onClick={async (e) => {
        const container = e.currentTarget.closest('[role="dialog"]') ?? document.body;
        const copied = await copyText(value, container);
        setState(copied ? "copied" : "failed");
        setTimeout(() => setState("idle"), copied ? 1500 : 4000);
        if (copied) return;
        const target = selectOnFail?.current;
        if (target) selectText(target);
        toast(target ? failedSelected : failed, "danger");
      }}
    >
      {state === "copied" ? (
        <Check aria-hidden="true" className="hg-enter text-ok" />
      ) : state === "failed" ? (
        <X aria-hidden="true" className="hg-enter text-danger" />
      ) : (
        <Copy aria-hidden="true" />
      )}
      {showLabel
        ? state === "copied"
          ? "Copied"
          : state === "failed"
            ? "Not copied"
            : label
        : null}
      {/* The toast region is outside open dialogs, which hide the rest of
          the page from screen readers, so the button announces it too. */}
      <span className="sr-only" aria-live="polite">
        {state === "copied"
          ? "Copied"
          : state === "failed"
            ? selectOnFail?.current
              ? failedSelected
              : failed
            : ""}
      </span>
    </Button>
  );
}

/**
 * A value the user must copy now, such as a new Token or a one-time link,
 * highlighted so it is not missed, with its copy button.
 */
export function CopyField({
  value,
  label,
  icon,
  small,
  testId,
}: {
  value: string;
  label: string;
  icon?: ReactNode;
  small?: boolean;
  testId?: string;
}) {
  const text = useRef<HTMLElement>(null);
  return (
    <div className="flex items-center gap-2 rounded-md border border-accent/40 bg-accent-soft py-2 pr-2 pl-3 [&>svg]:size-4 [&>svg]:shrink-0 [&>svg]:text-accent">
      {icon}
      <code
        ref={text}
        className={cn("min-w-0 flex-1 break-all font-mono text-ink", small ? "text-xs" : "text-sm")}
        data-testid={testId}
      >
        {value}
      </code>
      <CopyButton value={value} label={label} selectOnFail={text} />
    </div>
  );
}
