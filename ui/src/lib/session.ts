import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, setCsrfToken } from "@/api/client";
import type { Me } from "@/api/types";
import { ApiError, unwrap } from "./problem";

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

/** True when the user is an Instance admin. */
export function isAdmin(me: Me | null | undefined): boolean {
  return Boolean(me?.is_admin);
}

/** Namespaces the user can create Repos in: their own and Organisations where they can write. */
export function writableNamespaces(me: Me | null | undefined): string[] {
  if (!me) return [];
  return [me.username, ...me.orgs.filter((o) => o.role !== "read").map((o) => o.name)];
}
