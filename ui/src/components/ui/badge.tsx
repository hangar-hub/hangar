import { cva, type VariantProps } from "class-variance-authority";
import type { HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export const badgeVariants = cva(
  "inline-flex items-center gap-1 rounded-sm border px-1.5 py-px text-xs font-medium leading-5 whitespace-nowrap [&_svg]:size-3",
  {
    variants: {
      tone: {
        neutral: "border-line bg-raised text-muted",
        accent: "border-amber-700/30 bg-accent-soft text-accent",
        ok: "border-ok/30 bg-ok-soft text-ok",
        warn: "border-warn/30 bg-warn-soft text-warn",
        danger: "border-danger/30 bg-danger-soft text-danger",
        info: "border-info/30 bg-info-soft text-info",
      },
      mono: { true: "font-mono font-normal", false: "" },
    },
    defaultVariants: { tone: "neutral", mono: false },
  },
);

export function Badge({
  className,
  tone,
  mono,
  ...props
}: HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>) {
  return <span className={cn(badgeVariants({ tone, mono }), className)} {...props} />;
}
