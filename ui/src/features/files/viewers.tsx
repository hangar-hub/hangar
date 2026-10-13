import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api/client";
import { CodeView } from "@/components/content/CodeView";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/form";
import { ErrorState, LoadingRows } from "@/components/ui/states";
import { VirtualTable } from "@/components/VirtualTable";
import { formatContext, formatCount, formatParams } from "@/lib/format";
import { unwrap } from "@/lib/problem";
import { type RepoRef, repoPathParams } from "../repos/hooks";
import { ggufFileTypes, ggufSummary, safetensorsTensors, type TensorRow } from "./headers";

/** A collapsible JSON tree; native details elements make it keyboard accessible. */
export function JsonTree({
  value,
  name,
  depth = 0,
}: {
  value: unknown;
  name?: string;
  depth?: number;
}) {
  const label =
    name !== undefined ? <span className="text-accent">{JSON.stringify(name)}: </span> : null;
  if (value === null || typeof value !== "object") {
    const cls =
      typeof value === "string"
        ? "text-ok"
        : typeof value === "number"
          ? "text-info"
          : "text-muted";
    return (
      <div className="pl-4">
        {label}
        <span className={cls}>{JSON.stringify(value)}</span>
      </div>
    );
  }
  const entries = Array.isArray(value)
    ? value.map((v, i) => [String(i), v] as const)
    : Object.entries(value as Record<string, unknown>);
  const open = Array.isArray(value) ? "[" : "{";
  const close = Array.isArray(value) ? "]" : "}";
  return (
    <details open={depth < 2} className="pl-4">
      <summary className="-ml-4 cursor-pointer list-inside">
        {label}
        <span className="text-muted">
          {open} {entries.length}{" "}
          {Array.isArray(value)
            ? entries.length === 1
              ? "item"
              : "items"
            : entries.length === 1
              ? "key"
              : "keys"}{" "}
          {close}
        </span>
      </summary>
      {entries.map(([k, v]) => (
        <JsonTree key={k} name={Array.isArray(value) ? undefined : k} value={v} depth={depth + 1} />
      ))}
    </details>
  );
}

function useHeader(repo: RepoRef, rev: string, path: string) {
  return useQuery({
    queryKey: ["header", repo.kind, repo.ns, repo.name, rev, path],
    queryFn: async () =>
      unwrap(
        await api.GET("/repos/{type}/{ns}/{name}/files/{rev}/{path}/header", {
          params: { path: { ...repoPathParams(repo), rev, path } },
        }),
      ),
    staleTime: Number.POSITIVE_INFINITY,
  });
}

function TensorTable({ tensors, label }: { tensors: TensorRow[]; label: string }) {
  const [filter, setFilter] = useState("");
  const shown = filter
    ? tensors.filter((t) => t.name.toLowerCase().includes(filter.toLowerCase()))
    : tensors;
  return (
    <div className="space-y-2">
      <Input
        type="search"
        aria-label="Filter tensors by name"
        placeholder="Filter tensors"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        className="max-w-xs"
      />
      <VirtualTable
        label={label}
        rows={shown}
        template="minmax(220px,3fr) 90px minmax(120px,1.4fr) 110px"
        columns={[
          {
            header: "Tensor",
            cell: (t) => (
              <span className="font-mono" title={t.name}>
                {t.name}
              </span>
            ),
          },
          { header: "Type", cell: (t) => <span className="font-mono">{t.dtype}</span> },
          {
            header: "Shape",
            cell: (t) => <span className="font-mono">[{t.shape.join(", ")}]</span>,
          },
          {
            header: "Parameters",
            className: "text-right",
            cell: (t) => <span className="tabular font-mono">{formatCount(t.params)}</span>,
          },
        ]}
      />
    </div>
  );
}

function Stat({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="rounded-md border border-line bg-surface px-3 py-2">
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="mt-0.5 font-mono text-sm">{value}</dd>
    </div>
  );
}

/** The safetensors header viewer: tensors, shapes, dtypes and parameter totals, read without downloading the file. */
export function SafetensorsView({ repo, rev, path }: { repo: RepoRef; rev: string; path: string }) {
  const h = useHeader(repo, rev, path);
  if (h.isPending) return <LoadingRows rows={4} label="Reading the header" />;
  if (h.isError) return <ErrorState error={h.error} onRetry={() => h.refetch()} />;
  const s = safetensorsTensors(h.data.header);
  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        <Stat label="Parameters" value={`${formatParams(s.total)} (${formatCount(s.total)})`} />
        <Stat label="Tensors" value={formatCount(s.tensors.length)} />
        {Object.entries(s.byDtype).map(([dtype, n]) => (
          <Stat key={dtype} label={`${dtype} parameters`} value={formatParams(n)} />
        ))}
      </dl>
      {Object.keys(s.metadata).length ? (
        <section aria-labelledby="st-meta">
          <h3 id="st-meta" className="mb-1 text-sm font-semibold">
            Metadata
          </h3>
          <dl className="grid gap-x-4 font-mono text-xs sm:grid-cols-[max-content_1fr]">
            {Object.entries(s.metadata).map(([k, v]) => (
              <div key={k} className="contents">
                <dt className="text-muted">{k}</dt>
                <dd className="break-all">{String(v)}</dd>
              </div>
            ))}
          </dl>
        </section>
      ) : null}
      <TensorTable tensors={s.tensors} label="Tensors in this file" />
    </div>
  );
}

/** The GGUF viewer: metadata keys, tokenizer summary, chat template and tensor list. */
export function GgufView({ repo, rev, path }: { repo: RepoRef; rev: string; path: string }) {
  const h = useHeader(repo, rev, path);
  const [showAll, setShowAll] = useState(false);
  if (h.isPending) return <LoadingRows rows={4} label="Reading the header" />;
  if (h.isError) return <ErrorState error={h.error} onRetry={() => h.refetch()} />;
  const g = ggufSummary(h.data.header);
  const quant =
    g.fileType !== undefined ? (ggufFileTypes[g.fileType] ?? `file type ${g.fileType}`) : undefined;
  const meta = showAll ? g.metadata : g.metadata.slice(0, 30);
  return (
    <div className="space-y-5">
      <dl className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {g.architecture ? <Stat label="Architecture" value={g.architecture} /> : null}
        {quant ? <Stat label="Quantisation" value={<Badge mono>{quant}</Badge>} /> : null}
        {g.contextLength ? (
          <Stat label="Context length" value={formatContext(g.contextLength)} />
        ) : null}
        <Stat label="Parameters" value={g.total ? formatParams(g.total) : "Unknown"} />
        {g.version ? <Stat label="GGUF version" value={g.version} /> : null}
        <Stat label="Tensors" value={formatCount(g.tensors.length)} />
      </dl>
      <section aria-labelledby="gguf-tok">
        <h3 id="gguf-tok" className="mb-1 text-sm font-semibold">
          Tokenizer
        </h3>
        <dl className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <Stat label="Model" value={g.tokenizer.model ?? "Unknown"} />
          <Stat
            label="Vocabulary"
            value={
              g.tokenizer.vocabSize !== undefined ? formatCount(g.tokenizer.vocabSize) : "Unknown"
            }
          />
          <Stat label="BOS token ID" value={g.tokenizer.bos ?? "Not set"} />
          <Stat label="EOS token ID" value={g.tokenizer.eos ?? "Not set"} />
        </dl>
      </section>
      {g.chatTemplate ? (
        <section aria-labelledby="gguf-tpl">
          <h3 id="gguf-tpl" className="mb-1 text-sm font-semibold">
            Chat template
          </h3>
          <CodeView code={g.chatTemplate} lang="jinja" lineNumbers={false} />
        </section>
      ) : null}
      <section aria-labelledby="gguf-meta">
        <h3 id="gguf-meta" className="mb-1 text-sm font-semibold">
          Metadata
        </h3>
        <dl className="grid gap-x-4 gap-y-0.5 font-mono text-xs sm:grid-cols-[max-content_1fr]">
          {meta.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted">{k}</dt>
              <dd className="break-all">{v}</dd>
            </div>
          ))}
        </dl>
        {g.metadata.length > 30 && !showAll ? (
          <button
            type="button"
            className="mt-1 text-sm text-accent underline"
            onClick={() => setShowAll(true)}
          >
            Show all {g.metadata.length} keys
          </button>
        ) : null}
      </section>
      <section aria-labelledby="gguf-tensors">
        <h3 id="gguf-tensors" className="mb-1 text-sm font-semibold">
          Tensors
        </h3>
        <TensorTable tensors={g.tensors} label="Tensors in this file" />
      </section>
    </div>
  );
}
