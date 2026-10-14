import { useNavigate, useRouterState } from "@tanstack/react-router";
import { useCallback } from "react";

type SearchRecord = Record<string, string | undefined>;

/**
 * Reads and writes view state in the query string, so filters and tabs
 * make shareable URLs (09-web-ui.md "Global layout"). Empty values are
 * removed from the URL.
 */
export function useSearchState<T extends SearchRecord>(): [T, (patch: Partial<T>) => void] {
  const search = useRouterState({ select: (s) => s.location.search }) as unknown as T;
  const navigate = useNavigate();
  const update = useCallback(
    (patch: Partial<T>) => {
      navigate({
        to: ".",
        replace: true,
        search: (prev: Record<string, unknown>) => {
          const next: Record<string, unknown> = { ...prev, ...patch };
          for (const [k, v] of Object.entries(next)) {
            if (v === undefined || v === "" || v === null) delete next[k];
          }
          return next as never;
        },
      });
    },
    [navigate],
  );
  return [search, update];
}

/** Reads a string from the query string. */
export function searchString(value: unknown): string | undefined {
  if (typeof value === "string" && value !== "") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return undefined;
}
