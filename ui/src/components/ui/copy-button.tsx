import { Check, Copy } from "lucide-react";
import { type ReactNode, useState } from "react";
import { cn } from "@/lib/utils";
import { Button } from "./button";

/** Copies text to the clipboard and confirms it in place. */
export function CopyButton({
  value,
  label = "Copy",
  showLabel,
  size = "icon-sm",
}: {
  value: string;
  label?: string;
  showLabel?: boolean;
  size?: "icon-sm" | "sm";
}) {
  const [copied, setCopied] = useState(false);
  return (
    <Button
      variant="ghost"
      size={showLabel ? "sm" : size}
      aria-label={showLabel ? undefined : label}
      title={label}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value);
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        } catch {
          setCopied(false);
        }
      }}
    >
      {copied ? (
        <Check aria-hidden="true" className="hg-enter text-ok" />
      ) : (
        <Copy aria-hidden="true" />
      )}
      {showLabel ? (copied ? "Copied" : label) : null}
      <span className="sr-only" aria-live="polite">
        {copied ? "Copied" : ""}
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
  return (
    <div className="flex items-center gap-2 rounded-md border border-accent/40 bg-accent-soft py-2 pr-2 pl-3 [&>svg]:size-4 [&>svg]:shrink-0 [&>svg]:text-accent">
      {icon}
      <code
        className={cn("min-w-0 flex-1 break-all font-mono text-ink", small ? "text-xs" : "text-sm")}
        data-testid={testId}
      >
        {value}
      </code>
      <CopyButton value={value} label={label} />
    </div>
  );
}
