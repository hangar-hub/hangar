import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, setCsrfToken } from "@/api/client";
import type { Me } from "@/api/types";
import { ApiError, errorStatus, unwrap } from "./problem";

export const meKey = ["me"] as const;

/**
 * Loads the signed-in user, or null when signed out. The server answers 204
 * when nobody is signed in, and 401 for a session that has ended.
 */
export async function fetchMe(): Promise<Me | null> {
  const res = await api.GET("/me");
  if (res.response.status === 204 || res.response.status === 401) {
    setCsrfToken(undefined);
    return null;
  }
  const me = unwrap(res);
  setCsrfToken(me.csrf_token);
  return me;
}

/** The signed-in user. `data` is null when nobody is signed in. */
export function useMe() {
  return useQuery({ queryKey: meKey, queryFn: fetchMe, staleTime: 60_000, retry: false });
}

/** Signs out and clears every cached query. */
export function useSignOut() {
  const qc = useQueryClient();
  return async () => {
    const res = await api.POST("/auth/logout");
    if (!res.response.ok && res.response.status !== 401) {
      throw new ApiError(res.response.status);
    }
    setCsrfToken(undefined);
    qc.setQueryData(meKey, null);
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== meKey[0] });
  };
}

/**
 * Records a new sign-in: stores the user, drops every other cached query
 * (they were fetched as someone else) and keeps the header's observers.
 */
export function useSignedIn() {
  const qc = useQueryClient();
  return (me: Me) => {
    setCsrfToken(me.csrf_token);
    qc.setQueryData(meKey, me);
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== meKey[0] });
  };
}

/** The page to come back to after signing in: the current path and query. */
export function currentPage(): string {
  return `${window.location.pathname}${window.location.search}`;
}

/**
 * Handles a 401 from any query or mutation. When the cache holds a
 * signed-in user, a 401 can mean the session has ended: it expired, was
 * revoked from another browser, or the password changed. GET /me says
 * whether it has. If it has, the handler forgets the user and everything
 * fetched as them, and calls `toSignIn` with the page to come back to.
 * Signed-out visitors' 401s are left to the page, which asks them to sign
 * in where it makes sense.
 */
export function onSessionEnded(
  qc: QueryClient,
  toSignIn: (redirect: string) => void,
): (err: unknown) => Promise<void> {
  let checking = false;
  return async (err) => {
    if (errorStatus(err) !== 401 || checking) return;
    if (!qc.getQueryData<Me | null>(meKey)) return;
    if (window.location.pathname.startsWith("/auth/")) return;
    checking = true;
    try {
      const me = await fetchMe();
      if (me) {
        qc.setQueryData(meKey, me);
        return;
      }
      qc.setQueryData(meKey, null);
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== meKey[0] });
      toSignIn(currentPage());
    } catch {
      // GET /me failed, so the session may still be fine: leave it.
    } finally {
      checking = false;
    }
  };
}

/** True when the user is an Instance admin. */
export function isAdmin(me: Me | null | undefined): boolean {
  return Boolean(me?.is_admin);
}

/** Namespaces the user can create Repos in: their own and Organisations where they can write. */
export function writableNamespaces(me: Me | null | undefined): string[] {
  if (!me) return [];
  return [me.username, ...me.orgs.filter((o) => o.role !== "read").map((o) => o.name)];
}
