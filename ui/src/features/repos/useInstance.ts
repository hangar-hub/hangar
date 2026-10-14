import { useQuery } from "@tanstack/react-query";
import { api } from "@/api/client";
import { unwrap } from "@/lib/problem";

/**
 * Whether to load images from other hosts in cards (`ui.external_images`,
 * default on). Every visitor reads it from `GET /auth/providers`.
 */
export function useExternalImages(): boolean {
  const providers = useQuery({
    queryKey: ["auth", "providers"],
    queryFn: async () => unwrap(await api.GET("/auth/providers")),
    staleTime: 60_000,
  });
  return providers.data?.ui_external_images ?? true;
}

/** Enabled Upstreams, for Pull actions and Discover. */
export function useUpstreams(enabled = true) {
  return useQuery({
    queryKey: ["upstreams"],
    enabled,
    queryFn: async () => unwrap(await api.GET("/upstreams")),
    staleTime: 5 * 60_000,
  });
}
