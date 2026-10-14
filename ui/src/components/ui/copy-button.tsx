import { Check, Copy } from "lucide-react";
import { useState } from "react";
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
      {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
      {showLabel ? (copied ? "Copied" : label) : null}
      <span className="sr-only" aria-live="polite">
        {copied ? "Copied" : ""}
      </span>
    </Button>
  );
}
