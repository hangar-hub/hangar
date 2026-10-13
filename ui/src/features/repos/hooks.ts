import { useQuery } from "@tanstack/react-query";
import { api } from "@/api/client";
import { ApiError, unwrap } from "@/lib/problem";
import { apiType, type RepoKind, resolveUrl } from "@/lib/repo";

export type RepoRef = { kind: RepoKind; ns: string; name: string };

export function repoPathParams(r: RepoRef) {
  return { type: apiType(r.kind), ns: r.ns, name: r.name };
}

export const repoKey = (r: RepoRef) =>
  ["repo", apiType(r.kind), r.ns.toLowerCase(), r.name.toLowerCase()] as const;

/** The Repo, shared by every tab of a Repo page. */
export function useRepo(r: RepoRef) {
  return useQuery({
    queryKey: repoKey(r),
    queryFn: async () =>
      unwrap(await api.GET("/repos/{type}/{ns}/{name}", { params: { path: repoPathParams(r) } })),
  });
}

export function useRefs(r: RepoRef) {
  return useQuery({
    queryKey: [...repoKey(r), "refs"],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/refs", { params: { path: repoPathParams(r) } }),
      ),
  });
}

export function useMetadata(r: RepoRef, rev?: string) {
  return useQuery({
    queryKey: [...repoKey(r), "metadata", rev ?? ""],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/metadata", {
          params: { path: repoPathParams(r), query: rev ? { rev } : {} },
        }),
      ),
    refetchInterval: (q) => (q.state.data?.status === "pending" ? 5000 : false),
  });
}

export function useDependencies(r: RepoRef) {
  return useQuery({
    queryKey: [...repoKey(r), "dependencies"],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/dependencies", {
          params: { path: repoPathParams(r) },
        }),
      ),
  });
}

export function useQuants(r: RepoRef, enabled: boolean) {
  return useQuery({
    queryKey: [...repoKey(r), "quants"],
    enabled,
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/quants", { params: { path: repoPathParams(r) } }),
      ),
  });
}

export function useDatasetConfigs(r: RepoRef, rev?: string) {
  return useQuery({
    queryKey: [...repoKey(r), "configs", rev ?? ""],
    enabled: r.kind === "dataset",
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/datasets/{ns}/{name}/configs", {
          params: { path: { ns: r.ns, name: r.name }, query: rev ? { rev } : {} },
        }),
      ),
    refetchInterval: (q) =>
      q.state.data?.configs.some((c) => c.splits.some((s) => s.status === "pending"))
        ? 5000
        : false,
  });
}

/** Fetches a text file through its `resolve` URL; null when it does not exist. */
export async function fetchText(r: RepoRef, rev: string, path: string): Promise<string | null> {
  const res = await fetch(resolveUrl(r.kind, r.ns, r.name, rev, path), {
    credentials: "same-origin",
  });
  if (res.status === 404) return null;
  if (!res.ok)
    throw new ApiError(res.status, undefined, res.headers.get("X-Error-Message") ?? undefined);
  return res.text();
}

export function useTextFile(r: RepoRef, rev: string, path: string, enabled = true) {
  return useQuery({
    queryKey: [...repoKey(r), "text", rev, path],
    enabled,
    queryFn: () => fetchText(r, rev, path),
  });
}
