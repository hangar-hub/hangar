import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/api/client";
import type { InstanceSettings } from "@/api/types";
import { useToast } from "@/components/ui/toast";
import { unwrap } from "@/lib/problem";

/** Instance settings, shared by the Pull settings and Instance settings pages. */
export function useInstanceSettings() {
  const qc = useQueryClient();
  const toast = useToast();
  const settings = useQuery({
    queryKey: ["admin", "settings"],
    queryFn: async () => unwrap(await api.GET("/admin/settings")),
  });
  const save = useMutation({
    mutationFn: async (patch: InstanceSettings) => {
      const { oidc_providers: _ro, ...body } = patch;
      return unwrap(await api.PATCH("/admin/settings", { body }));
    },
    onSuccess: (s) => {
      qc.setQueryData(["admin", "settings"], s);
      toast("Settings saved. Every replica uses them within a few seconds.");
    },
  });
  return { settings, save };
}
