import { useMutation } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { api } from "@/api/client";
import type { Job } from "@/api/types";
import { AppLink, useGo } from "@/components/AppLink";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/form";
import { useToast } from "@/components/ui/toast";
import { shortSha } from "@/lib/format";
import { errorMessage, unwrap } from "@/lib/problem";
import { type RepoKind, treePath } from "@/lib/repo";
import { type RepoRef, repoPathParams, useRefs } from "../repos/hooks";

/** Picks a branch, tag or pull request; `build` gives the URL for a Revision. */
export function RefPicker({
  repo,
  rev,
  build,
}: {
  repo: RepoRef;
  rev: string;
  build: (rev: string) => string;
}) {
  const refs = useRefs(repo);
  const go = useGo();
  const d = refs.data;
  const known = d
    ? [...d.branches, ...d.tags].some((r) => r.name === rev) ||
      d.pull_requests.some((r) => r.ref === rev)
    : true;
  return (
    <label className="flex items-center gap-2">
      <span className="sr-only">Branch or tag</span>
      <Select
        compact
        className="max-w-56 font-mono"
        title={rev}
        value={rev}
        onChange={(e) => go(build(e.target.value))}
        disabled={!d}
      >
        {!known ? (
          <option value={rev}>{/^[0-9a-f]{40}$/.test(rev) ? shortSha(rev) : rev}</option>
        ) : null}
        {d ? (
          <>
            <optgroup label="Branches">
              {d.branches.map((b) => (
                <option key={b.ref} value={b.name}>
                  {b.name}
                </option>
              ))}
            </optgroup>
            {d.tags.length ? (
              <optgroup label="Tags">
                {d.tags.map((t) => (
                  <option key={t.ref} value={t.name}>
                    {t.name}
                  </option>
                ))}
              </optgroup>
            ) : null}
            {d.pull_requests.length ? (
              <optgroup label="Pull requests">
                {d.pull_requests.map((p) => (
                  <option key={p.ref} value={p.ref}>
                    {p.ref}
                  </option>
                ))}
              </optgroup>
            ) : null}
          </>
        ) : (
          <option value={rev}>{rev}</option>
        )}
      </Select>
    </label>
  );
}

/** The path to a folder or file as links, from the Repo root. */
export function Breadcrumbs({
  kind,
  ns,
  name,
  rev,
  path,
}: {
  kind: RepoKind;
  ns: string;
  name: string;
  rev: string;
  path: string;
}) {
  const parts = path.split("/").filter(Boolean);
  return (
    <nav aria-label="Breadcrumb" className="min-w-0 font-mono text-sm">
      <ol className="flex flex-wrap items-center gap-1">
        <li>
          <AppLink href={treePath(kind, ns, name, rev)} className="text-accent hover:underline">
            {name}
          </AppLink>
        </li>
        {parts.map((p, i) => {
          const sub = parts.slice(0, i + 1).join("/");
          const last = i === parts.length - 1;
          return (
            <li key={sub} className="flex items-center gap-1">
              <span aria-hidden="true" className="text-muted">
                /
              </span>
              {last ? (
                <span aria-current="page" className="break-all">
                  {p}
                </span>
              ) : (
                <AppLink
                  href={treePath(kind, ns, name, rev, sub)}
                  className="text-accent hover:underline"
                >
                  {p}
                </AppLink>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}

async function waitForJob(id: string): Promise<Job> {
  for (let i = 0; i < 150; i++) {
    const j = unwrap(await api.GET("/jobs/{id}", { params: { path: { id } } }));
    if (j.status === "succeeded" || j.status === "failed" || j.status === "cancelled") return j;
    await new Promise((r) => setTimeout(r, 2000));
  }
  throw new Error("The zip is taking a long time. Find it later in Jobs.");
}

/** Downloads a Revision, or one folder of it, as a zip through a `zip.export` Job (DOWN-5). */
export function ZipButton({ repo, rev, path }: { repo: RepoRef; rev: string; path?: string }) {
  const toast = useToast();
  const zip = useMutation({
    mutationFn: async () => {
      const job = unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/zip", {
          params: { path: repoPathParams(repo), query: { rev, ...(path ? { path } : {}) } },
        }),
      );
      const done = job.status === "succeeded" ? job : await waitForJob(job.id);
      if (done.status !== "succeeded") throw new Error(done.error || "The zip could not be made.");
      const url = done.result?.url;
      if (typeof url !== "string")
        throw new Error(
          "The zip is ready but has no download link. Ask an admin to check the Blob store.",
        );
      window.location.assign(url);
    },
    onError: (e) => toast(errorMessage(e), "danger"),
  });
  return (
    <Button size="sm" onClick={() => zip.mutate()} disabled={zip.isPending}>
      <Download aria-hidden="true" />
      {zip.isPending ? "Preparing zip" : path ? "Download folder" : "Download zip"}
    </Button>
  );
}
