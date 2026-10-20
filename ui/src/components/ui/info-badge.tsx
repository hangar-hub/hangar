import type { VariantProps } from "class-variance-authority";
import { ArrowUpRight } from "lucide-react";
import { Popover } from "radix-ui";
import {
  type MouseEvent,
  type PointerEvent,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";
import { docsLink } from "@/lib/docs";
import { cn } from "@/lib/utils";
import { badgeVariants } from "./badge";

/** What a badge means, shown when the user hovers or clicks it. */
export type Explanation = {
  title: string;
  body: ReactNode;
  /**
   * The documentation page that says more, such as `concepts/mirrors/`.
   * The link is left out when the Instance has no docs links (ui.docs_url).
   */
  docs?: string;
};

// Hovering waits a moment so passing the pointer over a row of badges does
// not flash explanations. Once one is open, or was open a moment ago, the
// next opens at once and without its entrance motion.
const openDelay = 350;
const closeDelay = 150;
const warmFor = 400;
let openCount = 0;
let lastClosedAt = 0;

function isWarm() {
  return openCount > 0 || Date.now() - lastClosedAt < warmFor;
}

/**
 * A badge that explains itself. With a mouse, hovering opens the
 * explanation after a short delay and leaving closes it. Clicking, tapping
 * or pressing Enter opens it and keeps it open until the user clicks away
 * or presses Escape, so it works on touch screens and from the keyboard.
 */
export function InfoBadge({
  explanation,
  tone,
  mono,
  className,
  children,
}: {
  explanation: Explanation;
  className?: string;
  children: ReactNode;
} & VariantProps<typeof badgeVariants>) {
  const [open, setOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const [instant, setInstant] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const href = explanation.docs ? docsLink(explanation.docs) : undefined;

  useEffect(() => {
    if (!open) return;
    openCount++;
    return () => {
      openCount--;
      lastClosedAt = Date.now();
    };
  }, [open]);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  const change = (next: boolean, pin: boolean) => {
    window.clearTimeout(timer.current);
    setOpen(next);
    setPinned(next && pin);
  };
  const hoverIn = (e: PointerEvent) => {
    if (e.pointerType !== "mouse") return;
    window.clearTimeout(timer.current);
    if (open) return;
    if (isWarm()) {
      setInstant(true);
      change(true, false);
      return;
    }
    timer.current = window.setTimeout(() => {
      setInstant(false);
      change(true, false);
    }, openDelay);
  };
  const hoverOut = (e: PointerEvent) => {
    if (e.pointerType !== "mouse") return;
    window.clearTimeout(timer.current);
    if (pinned) return;
    timer.current = window.setTimeout(() => change(false, false), closeDelay);
  };
  // A click pins an explanation that hovering opened, rather than closing it.
  const click = (e: MouseEvent) => {
    e.preventDefault();
    setInstant(false);
    if (open && !pinned) change(true, true);
    else change(!open, true);
  };

  return (
    <Popover.Root open={open} onOpenChange={(next) => change(next, true)}>
      <Popover.Trigger
        type="button"
        onClick={click}
        onPointerEnter={hoverIn}
        onPointerLeave={hoverOut}
        className={cn(
          badgeVariants({ tone, mono }),
          "relative cursor-help transition-[border-color,scale] duration-150 ease-out hover:border-current/45 active:scale-[0.97] data-[state=open]:border-current/45",
          className,
        )}
      >
        {children}
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          side="bottom"
          align="start"
          sideOffset={6}
          collisionPadding={16}
          data-instant={instant || undefined}
          onPointerEnter={hoverIn}
          onPointerLeave={hoverOut}
          // Hover is a preview: it neither takes focus nor hands it back.
          onOpenAutoFocus={(e) => {
            if (!pinned) e.preventDefault();
          }}
          onCloseAutoFocus={(e) => {
            if (!pinned) e.preventDefault();
          }}
          className="hg-popover z-50 w-72 max-w-[calc(100vw-2rem)] rounded-md border border-line bg-surface p-3 text-sm text-ink shadow-lg focus:outline-none"
        >
          <p className="font-medium">{explanation.title}</p>
          <div className="mt-1 space-y-1.5 text-muted [&_code]:rounded-sm [&_code]:bg-raised [&_code]:px-1 [&_code]:font-mono [&_code]:text-[0.85em] [&_code]:text-ink">
            {explanation.body}
          </div>
          {href ? (
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              className="hg-link mt-2 inline-flex items-center gap-0.5 text-xs"
            >
              Read more in the docs
              <ArrowUpRight className="size-3" aria-hidden="true" />
            </a>
          ) : null}
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  );
}
