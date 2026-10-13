import { useRouter } from "@tanstack/react-router";
import type { AnchorHTMLAttributes, MouseEvent } from "react";

/**
 * A link to a computed path inside the UI, such as a Repo or file URL
 * built by lib/repo.ts. It navigates without a page load, and keeps the
 * browser's own behaviour for new tabs and windows.
 */
export function AppLink({
  href,
  onClick,
  ...props
}: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) {
  const router = useRouter();
  return (
    <a
      href={href}
      onClick={(e: MouseEvent<HTMLAnchorElement>) => {
        onClick?.(e);
        if (
          e.defaultPrevented ||
          e.metaKey ||
          e.ctrlKey ||
          e.shiftKey ||
          e.altKey ||
          e.button !== 0
        )
          return;
        if (props.target && props.target !== "_self") return;
        e.preventDefault();
        router.history.push(href);
      }}
      {...props}
    />
  );
}

/** Navigates to a computed path. */
export function useGo() {
  const router = useRouter();
  return (href: string, opts: { replace?: boolean } = {}) =>
    opts.replace ? router.history.replace(href) : router.history.push(href);
}
