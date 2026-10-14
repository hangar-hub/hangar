import {
  CloudDownload,
  Globe,
  Lock,
  ShieldAlert,
  ShieldCheck,
  ShieldQuestion,
  Users,
} from "lucide-react";
import type { ArsenalState, Visibility } from "@/api/types";
import { Badge } from "@/components/ui/badge";

export type ScanStatus = "safe" | "suspicious" | "unsafe" | "error" | "pending";

const scanText: Record<ScanStatus, string> = {
  safe: "Safe",
  suspicious: "Suspicious imports",
  unsafe: "Unsafe imports",
  error: "Scan failed",
  pending: "Scanning",
};

/** The pickle scan result of a file or Repo (META-2). */
export function ScanBadge({ status, hideSafe }: { status?: ScanStatus; hideSafe?: boolean }) {
  if (!status || (hideSafe && status === "safe")) return null;
  const tone =
    status === "safe"
      ? "ok"
      : status === "unsafe"
        ? "danger"
        : status === "suspicious"
          ? "warn"
          : "neutral";
  const Icon =
    status === "safe"
      ? ShieldCheck
      : status === "pending" || status === "error"
        ? ShieldQuestion
        : ShieldAlert;
  return (
    <Badge tone={tone}>
      <Icon aria-hidden="true" />
      {scanText[status]}
    </Badge>
  );
}

/** Visibility, with an icon and words so colour is never the only signal. */
export function VisibilityBadge({ visibility }: { visibility: Visibility }) {
  const Icon = visibility === "public" ? Globe : visibility === "internal" ? Users : Lock;
  const label =
    visibility === "public" ? "Public" : visibility === "internal" ? "Internal" : "Private";
  return (
    <Badge tone={visibility === "private" ? "warn" : "neutral"}>
      <Icon aria-hidden="true" />
      {label}
    </Badge>
  );
}

/** "Mirrored from huggingface.co" (01-product.md copy table). */
export function MirrorBadge({ upstream, url }: { upstream?: string; url?: string }) {
  let host = upstream;
  if (url) {
    try {
      host = new URL(url).host;
    } catch {
      host = upstream;
    }
  }
  return (
    <Badge tone="info">
      <CloudDownload aria-hidden="true" />
      Mirrored from {host || "an Upstream"}
    </Badge>
  );
}

/** A Discover result's state in the Arsenal (BROWSE-2). */
export function ArsenalBadge({
  state,
  updateAvailable,
}: {
  state: ArsenalState;
  updateAvailable?: boolean;
}) {
  if (updateAvailable) return <Badge tone="accent">Update available</Badge>;
  if (state === "full") return <Badge tone="ok">In arsenal</Badge>;
  if (state === "partial") return <Badge tone="accent">Partly in arsenal</Badge>;
  return null;
}

/** "Not in arsenal" for files a Partial mirror does not hold (PULL-3). */
export function NotPresentBadge() {
  return <Badge tone="neutral">Not in arsenal</Badge>;
}

const formatNames: Record<string, string> = {
  gguf: "GGUF",
  safetensors: "safetensors",
  mlx: "MLX",
  onnx: "ONNX",
  pytorch: "PyTorch",
  tensorflow: "TensorFlow",
  flax: "Flax",
  coreml: "Core ML",
  openvino: "OpenVINO",
  tflite: "TFLite",
  parquet: "Parquet",
  csv: "CSV",
  json: "JSON",
};

export function formatName(f: string): string {
  return formatNames[f.toLowerCase()] ?? f;
}

export function FormatBadges({ formats }: { formats?: string[] }) {
  if (!formats?.length) return null;
  return (
    <>
      {formats.map((f) => (
        <Badge key={f}>{formatName(f)}</Badge>
      ))}
    </>
  );
}

/** Quantisation chips, in monospace (09-web-ui.md "Visual direction"). */
export function QuantChips({ quants, max = 4 }: { quants?: string[]; max?: number }) {
  if (!quants?.length) return null;
  const shown = quants.slice(0, max);
  return (
    <>
      {shown.map((q) => (
        <Badge key={q} mono>
          {q}
        </Badge>
      ))}
      {quants.length > max ? <Badge mono>+{quants.length - max}</Badge> : null}
    </>
  );
}
