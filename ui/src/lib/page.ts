import { useRouterState } from "@tanstack/react-router";
import { useEffect, useRef } from "react";

/** The browser tab's text for a page: "Tokens · Hangar", or "Hangar" alone. */
export function documentTitle(page?: string): string {
  const name = page?.trim();
  return name ? `${name} · Hangar` : "Hangar";
}

/**
 * Sets the browser tab's text while the page is shown, so each page has a
 * title that says what it is (WCAG 2.4.2) and screen readers announce it.
 * Going back to "Hangar" on unmount covers pages that do not set one.
 */
export function useDocumentTitle(page?: string): void {
  useEffect(() => {
    document.title = documentTitle(page);
    return () => {
      document.title = documentTitle();
    };
  }, [page]);
}

/**
 * Moves focus to the new page's main heading, or to the main landmark while
 * the heading has not rendered, after each navigation to another path. A
 * client-side navigation otherwise leaves focus on the link that was used,
 * or on nothing, and screen readers announce nothing. Query string changes,
 * such as filters and tabs, keep focus where it is.
 */
export function useFocusOnNavigation(): void {
  const pathname = useRouterState({
    select: (s) => s.resolvedLocation?.pathname ?? s.location.pathname,
  });
  const last = useRef(pathname);
  useEffect(() => {
    if (last.current === pathname) return;
    last.current = pathname;
    const frame = requestAnimationFrame(() => focusMainHeading());
    return () => cancelAnimationFrame(frame);
  }, [pathname]);
}

/** Focuses the page's h1, or the main landmark when there is none yet. */
export function focusMainHeading(): void {
  const target = document.getElementById("page-title") ?? document.getElementById("main");
  if (!target) return;
  if (!target.hasAttribute("tabindex")) target.setAttribute("tabindex", "-1");
  target.focus({ preventScroll: true });
}
