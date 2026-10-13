import { PageShell } from "@/components/PageShell";

export type RepoKind = "model" | "dataset";

/** Returns the Repo ID as shown in the UI, with the datasets/ prefix for datasets. */
export function repoLabel(kind: RepoKind, ns: string, name: string): string {
  return `${kind === "dataset" ? "datasets/" : ""}${ns}/${name}`;
}

/** The frame of every Repo page: the Repo ID and the page name. */
export function RepoPageShell(props: {
  kind: RepoKind;
  ns: string;
  name: string;
  page: string;
  detail?: string;
}) {
  const id = repoLabel(props.kind, props.ns, props.name);
  return <PageShell title={props.page} subtitle={props.detail ? `${id} · ${props.detail}` : id} />;
}
