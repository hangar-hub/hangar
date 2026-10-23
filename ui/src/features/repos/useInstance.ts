import { useQuery } from "@tanstack/react-query";
import { api } from "@/api/client";
import { unwrap } from "@/lib/problem";

/**
 * Whether to load images from other hosts in cards (`ui.external_images`).
 * Every visitor reads it from `GET /auth/providers`. Until the setting is
 * known, and if it cannot be read, the answer is undefined, which Markdown
 * treats as off: an air-gapped Instance must not reach the internet
 * because a request failed or has not answered yet.
 */
export function useExternalImages(): boolean | undefined {
  const providers = useQuery({
    queryKey: ["auth", "providers"],
    queryFn: async () => unwrap(await api.GET("/auth/providers")),
    staleTime: 60_000,
  });
  return providers.data?.ui_external_images;
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
