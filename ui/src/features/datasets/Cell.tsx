import { useState } from "react";
import type { FeatureType } from "@/api/viewer";
import { ScrollRegion } from "@/components/ScrollRegion";
import { formatCount } from "@/lib/format";

function isImage(v: unknown): v is { src: string; width?: number; height?: number } {
  return typeof v === "object" && v !== null && typeof (v as { src?: unknown }).src === "string";
}

function audioSources(v: unknown): { src: string; type?: string }[] | null {
  if (
    Array.isArray(v) &&
    v.every(
      (x) =>
        typeof x === "object" && x !== null && typeof (x as { src?: unknown }).src === "string",
    )
  ) {
    return v as { src: string; type?: string }[];
  }
  return null;
}

function TextCell({ text, truncated }: { text: string; truncated: boolean }) {
  const [open, setOpen] = useState(false);
  const long = text.length > 160 || text.includes("\n");
  if (!long) return <span className="whitespace-pre-wrap break-words">{text}</span>;
  return (
    <div>
      <span
        className={
          open ? "whitespace-pre-wrap break-words" : "line-clamp-3 whitespace-pre-wrap break-words"
        }
      >
        {text}
      </span>
      <button
        type="button"
        className="mt-0.5 text-xs text-accent underline"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {open ? "Show less" : "Show more"}
      </button>
      {open && truncated ? (
        <p className="mt-1 text-xs text-muted">
          The server shortened this cell. Download the Parquet file for the full value.
        </p>
      ) : null}
    </div>
  );
}

/** One table cell, rendered by its feature type (DATA-6). */
export function Cell({
  value,
  type,
  truncated,
  label,
}: {
  value: unknown;
  type: FeatureType;
  truncated: boolean;
  label: string;
}) {
  if (value === null || value === undefined)
    return <span className="text-xs text-muted italic">null</span>;
  if (type._type === "ClassLabel" && typeof value === "number") {
    const name = type.names?.[value];
    return (
      <span>
        {name ?? "Unknown label"} <span className="font-mono text-xs text-muted">({value})</span>
      </span>
    );
  }
  if (type._type === "Image" && isImage(value)) {
    return (
      <a
        href={value.src}
        target="_blank"
        rel="noopener noreferrer"
        aria-label={`Open the full-size image for ${label}`}
      >
        <img
          src={value.src}
          alt={label}
          loading="lazy"
          className="max-h-24 max-w-32 rounded-sm border border-line object-contain"
        />
      </a>
    );
  }
  const audio = type._type === "Audio" ? audioSources(value) : null;
  if (audio) {
    return (
      // biome-ignore lint/a11y/useMediaCaption: dataset audio has no captions to offer
      <audio controls preload="none" className="h-8 max-w-60" aria-label={label}>
        {audio.map((s) => (
          <source key={s.src} src={s.src} type={s.type} />
        ))}
      </audio>
    );
  }
  if (typeof value === "number")
    return (
      <span className="tabular font-mono">
        {Number.isInteger(value) ? formatCount(value) : value}
      </span>
    );
  if (typeof value === "boolean") return <span className="font-mono">{String(value)}</span>;
  if (typeof value === "string") return <TextCell text={value} truncated={truncated} />;
  const json = JSON.stringify(value, null, 2);
  const summary = Array.isArray(value)
    ? `List of ${value.length}`
    : `${Object.keys(value as object).length} fields`;
  return (
    <details>
      <summary className="cursor-pointer text-xs text-muted">{summary}</summary>
      <ScrollRegion label={`${label} value`} className="mt-1 max-h-60">
        <pre className="font-mono text-xs">{json}</pre>
      </ScrollRegion>
    </details>
  );
}
