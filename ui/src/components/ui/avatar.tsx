import { cn } from "@/lib/utils";

/**
 * A user's initial in a tile, for the user menu and comment threads. It is
 * decorative: the username is always written next to it or in a label.
 */
export function Avatar({
  name,
  size = "md",
  className,
}: {
  name: string;
  size?: "sm" | "md";
  className?: string;
}) {
  return (
    <span
      aria-hidden="true"
      className={cn(
        "grid shrink-0 select-none place-items-center rounded-sm bg-steel-700 font-mono text-xs uppercase text-steel-50",
        size === "sm" ? "size-6" : "size-7",
        className,
      )}
    >
      {name.slice(0, 1)}
    </span>
  );
}
