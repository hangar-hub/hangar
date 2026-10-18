import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/**
 * A set of toggle buttons in one frame, of which one is pressed, for
 * filters such as a list's type or status.
 */
export function SegmentedControl<T>({
  legend,
  value,
  onValueChange,
  options,
  className,
}: {
  legend: string;
  value: T;
  onValueChange: (v: T) => void;
  options: { value: T; label: ReactNode }[];
  className?: string;
}) {
  return (
    <fieldset
      className={cn("flex gap-0.5 rounded-md border border-line bg-surface p-0.5", className)}
    >
      <legend className="sr-only">{legend}</legend>
      {options.map((o) => {
        const on = o.value === value;
        return (
          <button
            key={String(o.value)}
            type="button"
            aria-pressed={on}
            onClick={() => onValueChange(o.value)}
            className={cn(
              "rounded-sm px-2.5 py-1 text-sm transition-[color,background-color,scale] duration-150 ease-out active:scale-[0.98]",
              on
                ? "bg-raised font-medium text-ink"
                : "text-muted hover:bg-raised/60 hover:text-ink",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </fieldset>
  );
}
