import { useQuery } from "@tanstack/react-query";
import { api } from "@/api/client";
import { unwrap } from "@/lib/problem";
import { useMe } from "@/lib/session";

/**
 * Whether to load images from other hosts in cards (`ui.external_images`,
 * default on). Only admins can read Instance settings through the native
 * API, so other users get the default (see docs/build/requests/w8.md).
 */
export function useExternalImages(): boolean {
  const me = useMe();
  const settings = useQuery({
    queryKey: ["admin", "settings"],
    enabled: Boolean(me.data?.is_admin),
    queryFn: async () => unwrap(await api.GET("/admin/settings")),
  });
  return settings.data?.ui_external_images ?? true;
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
