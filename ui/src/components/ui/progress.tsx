import { percent } from "@/lib/format";
import { cn } from "@/lib/utils";

/** A progress bar with an accessible value. */
export function ProgressBar({
  done,
  total,
  label,
  className,
}: {
  done: number;
  total: number;
  label: string;
  className?: string;
}) {
  const p = percent(done, total);
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(p)}
      className={cn("h-1.5 w-full overflow-hidden rounded-full bg-raised", className)}
    >
      <div
        className="h-full w-full rounded-full bg-accent-fill transition-transform duration-300 ease-out"
        style={{ transform: `translateX(${p - 100}%)` }}
      />
    </div>
  );
}
