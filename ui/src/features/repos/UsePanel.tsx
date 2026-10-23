import { Code2 } from "lucide-react";
import { useState } from "react";
import type { Repo } from "@/api/types";
import { CodeView } from "@/components/content/CodeView";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Dialog } from "@/components/ui/dialog";
import { Tabs } from "@/components/ui/tabs";
import { errorStatus } from "@/lib/problem";
import { snippets } from "@/lib/snippets";
import { useDatasetConfigs, useDependencies, useQuants } from "./hooks";

/**
 * The **Use this model** panel, with a tab and copy button per client.
 * `revision` is the branch, tag or Commit being viewed, when it is not the
 * default branch, so the snippets fetch what is on screen.
 */
export function UsePanel({ repo, revision }: { repo: Repo; revision?: string }) {
  const [open, setOpen] = useState(false);
  const ref = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const isGguf = Boolean(repo.summary?.formats?.includes("gguf"));
  const quants = useQuants(ref, open && isGguf);
  const deps = useDependencies(ref);
  const configs = useDatasetConfigs(ref);
  const firstConfig = configs.data?.configs.find((c) => c.default) ?? configs.data?.configs[0];
  const list = snippets({
    kind: repo.type,
    id: repo.id,
    origin: window.location.origin,
    revision,
    library: repo.summary?.library_name,
    formats: repo.summary?.formats,
    quants: quants.data?.items.map((g) => g.name) ?? repo.summary?.quants,
    remoteCode: Boolean(deps.data?.needs.some((d) => d.kind === "code")),
    config: firstConfig?.config,
    split: firstConfig?.splits[0]?.split,
    defaultBranch: repo.default_branch,
    // 422: the dataset has no files the datasets library reads.
    readable: !(configs.isError && errorStatus(configs.error) === 422),
  });
  const [tab, setTab] = useState(list[0]?.id ?? "");
  const label = repo.type === "dataset" ? "Use this dataset" : "Use this model";
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      wide
      title={label}
      description={
        revision
          ? `Snippets for ${repo.id} at ${revision} on this Instance.`
          : `Snippets for ${repo.id} on this Instance.`
      }
      trigger={
        <Button variant="primary" size="sm">
          <Code2 aria-hidden="true" />
          {label}
        </Button>
      }
    >
      <Tabs
        label="Clients"
        value={list.some((s) => s.id === tab) ? tab : (list[0]?.id ?? "")}
        onValueChange={setTab}
        tabs={list.map((s) => ({
          value: s.id,
          label: s.label,
          content: (
            <div className="relative">
              <div className="absolute top-1.5 right-1.5 z-10">
                <CopyButton value={s.code} label={`Copy the ${s.label} snippet`} />
              </div>
              <CodeView code={s.code} lang={s.language} lineNumbers={false} />
            </div>
          ),
        }))}
      />
    </Dialog>
  );
}
