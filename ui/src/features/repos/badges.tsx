import {
  CloudDownload,
  Globe,
  Lock,
  ShieldAlert,
  ShieldCheck,
  ShieldQuestion,
  Users,
} from "lucide-react";
import type { ArsenalState, Dependency, Role, Visibility } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { type Explanation, InfoBadge } from "@/components/ui/info-badge";

export type ScanStatus = "safe" | "suspicious" | "unsafe" | "error" | "pending";

const scanText: Record<ScanStatus, string> = {
  safe: "Safe",
  suspicious: "Suspicious imports",
  unsafe: "Unsafe imports",
  error: "Scan failed",
  pending: "Scanning",
};

const scanDocs = "operate/security/#choose-what-to-block";

/**
 * What a scan result means. A Repo's result is its worst file's, so the
 * Repo wording says where to look for the file.
 */
function scanExplanation(status: ScanStatus, scope: "repo" | "file"): Explanation {
  const where = scope === "repo" ? "at least one file in this Repo" : "this file";
  const find =
    scope === "repo" ? (
      <p>The Files tab marks which files, and each file's page lists the imports.</p>
    ) : null;
  switch (status) {
    case "safe":
      return {
        title: "No risky imports found",
        body: (
          <p>
            {scope === "repo"
              ? "Every pickle file in this Repo imports only functions that rebuild tensors and containers. Formats such as safetensors and GGUF hold only data and cannot run code."
              : "This file imports only functions that rebuild tensors and containers, or is not a pickle at all."}
          </p>
        ),
        docs: scanDocs,
      };
    case "suspicious":
      return {
        title: "Suspicious imports",
        body: (
          <>
            <p>
              Pickle files (such as <code>.bin</code>, <code>.pt</code> and <code>.pkl</code>) can
              run code when they are loaded. The scan found imports in {where} that are on neither
              its list of known-safe functions nor its list of dangerous ones, such as a model class
              from scikit-learn or transformers.
            </p>
            <p>
              These are often harmless, but check where the file came from before you load it, or
              load it with <code>torch.load(weights_only=True)</code>. Prefer a safetensors copy.
            </p>
            {find}
          </>
        ),
        docs: scanDocs,
      };
    case "unsafe":
      return {
        title: "Unsafe imports",
        body: (
          <>
            <p>
              Pickle files (such as <code>.bin</code>, <code>.pt</code> and <code>.pkl</code>) can
              run code when they are loaded. The scan found imports in {where} that can run commands
              or reach the system, such as <code>os.system</code>, <code>subprocess</code>,{" "}
              <code>builtins.eval</code> or <code>socket</code>.
            </p>
            <p>
              Do not load it with <code>pickle</code> or <code>torch.load</code> unless you trust
              where it came from. Prefer a safetensors copy.
            </p>
            {find}
          </>
        ),
        docs: scanDocs,
      };
    case "error":
      return {
        title: "Scan failed",
        body: (
          <p>
            The scan could not read {where} to the end, for example because it is compressed with xz
            or cut short. Hangar cannot say whether it runs code when loaded, so treat it like a
            pickle nobody has checked.
          </p>
        ),
        docs: scanDocs,
      };
    case "pending":
      return {
        title: "Scan in progress",
        body: (
          <p>
            Hangar is still checking pickle files for imports that can run code. Wait for the result
            before you load them.
          </p>
        ),
        docs: scanDocs,
      };
  }
}

/**
 * The pickle scan result of a file or Repo (META-2). It explains itself on
 * hover or click unless `plain`, for places that already explain it.
 */
export function ScanBadge({
  status,
  hideSafe,
  scope = "repo",
  plain,
}: {
  status?: ScanStatus;
  hideSafe?: boolean;
  scope?: "repo" | "file";
  plain?: boolean;
}) {
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
  const content = (
    <>
      <Icon aria-hidden="true" />
      {scanText[status]}
    </>
  );
  if (plain) return <Badge tone={tone}>{content}</Badge>;
  return (
    <InfoBadge tone={tone} explanation={scanExplanation(status, scope)}>
      {content}
    </InfoBadge>
  );
}

const permissionsDocs = "concepts/permissions/";

const visibilityExplanations: Record<Visibility, Explanation> = {
  public: {
    title: "Public",
    body: (
      <p>
        Anyone who can reach this Instance can see it and download from it. If the Instance allows
        anonymous reads, that includes people who are not signed in.
      </p>
    ),
    docs: permissionsDocs,
  },
  internal: {
    title: "Internal",
    body: <p>Anyone signed in to this Instance can see it and download from it.</p>,
    docs: permissionsDocs,
  },
  private: {
    title: "Private",
    body: (
      <p>
        Only Members of its Namespace, its collaborators and Instance admins can see it. To everyone
        else it looks as if it does not exist.
      </p>
    ),
    docs: permissionsDocs,
  },
};

/** Visibility, with an icon and words so colour is never the only signal. */
export function VisibilityBadge({ visibility }: { visibility: Visibility }) {
  const Icon = visibility === "public" ? Globe : visibility === "internal" ? Users : Lock;
  const label =
    visibility === "public" ? "Public" : visibility === "internal" ? "Internal" : "Private";
  return (
    <InfoBadge
      tone={visibility === "private" ? "warn" : "neutral"}
      explanation={visibilityExplanations[visibility]}
    >
      <Icon aria-hidden="true" />
      {label}
    </InfoBadge>
  );
}

/** A gated Repo (REPO-8). */
export function GatedBadge() {
  return (
    <InfoBadge
      tone="warn"
      explanation={{
        title: "Gated",
        body: (
          <p>
            Anyone who can see this Repo can read its card, but downloading its files needs an
            access request that an admin of the Repo accepts.
          </p>
        ),
        docs: "guides/gated/",
      }}
    >
      Gated
    </InfoBadge>
  );
}

const mirrorDocs = "concepts/mirrors/";

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
    <InfoBadge
      tone="info"
      explanation={{
        title: "Mirror",
        body: (
          <>
            <p>
              This Repo tracks a Repo on {host || "an Upstream"}, with the same Commits. Syncing
              fetches Commits added there since.
            </p>
            <p>
              A Mirror can hold every file, or only the ones pulled, such as one quantisation. Files
              it does not hold are marked Not in arsenal.
            </p>
          </>
        ),
        docs: mirrorDocs,
      }}
    >
      <CloudDownload aria-hidden="true" />
      Mirrored from {host || "an Upstream"}
    </InfoBadge>
  );
}

/** A Mirror whose Upstream has Commits it does not. */
export function UpdateAvailableBadge() {
  return (
    <InfoBadge
      tone="accent"
      explanation={{
        title: "Update available",
        body: (
          <p>
            The Upstream has Commits this Instance does not have yet. Syncing the Mirror fetches
            them.
          </p>
        ),
        docs: mirrorDocs,
      }}
    >
      Update available
    </InfoBadge>
  );
}

const arsenalExplanations: Record<"full" | "partial", Explanation> = {
  full: {
    title: "In arsenal",
    body: <p>This Instance already holds every file of this Repo, so it downloads from here.</p>,
    docs: mirrorDocs,
  },
  partial: {
    title: "Partly in arsenal",
    body: (
      <p>
        This Instance holds some of this Repo's files, such as one quantisation. Pull more when you
        need them.
      </p>
    ),
    docs: mirrorDocs,
  },
};

/** A Discover result's state in the Arsenal (BROWSE-2). */
export function ArsenalBadge({
  state,
  updateAvailable,
}: {
  state: ArsenalState;
  updateAvailable?: boolean;
}) {
  if (updateAvailable) return <UpdateAvailableBadge />;
  if (state === "full")
    return (
      <InfoBadge tone="ok" explanation={arsenalExplanations.full}>
        In arsenal
      </InfoBadge>
    );
  if (state === "partial")
    return (
      <InfoBadge tone="accent" explanation={arsenalExplanations.partial}>
        Partly in arsenal
      </InfoBadge>
    );
  return null;
}

/** "Not in arsenal" for files a Partial mirror does not hold (PULL-3). */
export function NotPresentBadge() {
  return (
    <InfoBadge
      tone="neutral"
      explanation={{
        title: "Not in arsenal",
        body: (
          <p>
            This Mirror has the file's history but not its content. Pull it to store it on this
            Instance.
          </p>
        ),
        docs: mirrorDocs,
      }}
    >
      Not in arsenal
    </InfoBadge>
  );
}

/** A file stored in Git LFS, with its SHA-256 when known. */
export function LfsBadge({ oid }: { oid?: string }) {
  return (
    <InfoBadge
      mono
      explanation={{
        title: "Stored with Git LFS",
        body: (
          <>
            <p>
              Git holds a small pointer to this file, and its content is stored once in the
              Instance's Blob store. <code>git clone</code> fetches it through git-lfs; the Hub API
              serves it directly.
            </p>
            {oid ? <p className="break-all font-mono text-xs">SHA-256 {oid}</p> : null}
          </>
        ),
        docs: "guides/git/",
      }}
    >
      LFS
    </InfoBadge>
  );
}

const dependencyDocs = "concepts/pulls/";

const dependencyExplanations: Record<Dependency["kind"], Explanation> = {
  code: {
    title: "Code",
    body: (
      <p>
        This model's config loads Python classes from that Repo (<code>auto_map</code>), so it
        cannot load without it. Hangar pulls it automatically.
      </p>
    ),
    docs: dependencyDocs,
  },
  base_model: {
    title: "Base model",
    body: <p>The model this one was built from. Suggested, not pulled automatically.</p>,
    docs: dependencyDocs,
  },
  adapter_base: {
    title: "Adapter base",
    body: (
      <p>
        This Repo is an adapter, such as a LoRA, that runs on top of that base model. You need both
        to use it.
      </p>
    ),
    docs: dependencyDocs,
  },
  card_base_model: {
    title: "Base model",
    body: (
      <p>
        The model card names this as the model it was built from. Suggested, not pulled
        automatically.
      </p>
    ),
    docs: dependencyDocs,
  },
  dataset: {
    title: "Dataset",
    body: <p>The model card lists this dataset. It is shown for reference and not pulled.</p>,
    docs: dependencyDocs,
  },
};

/** What kind of Dependency a Repo is, explained. */
export function DependencyKindBadge({ kind, label }: { kind: Dependency["kind"]; label: string }) {
  return <InfoBadge explanation={dependencyExplanations[kind]}>{label}</InfoBadge>;
}

/** Whether a Dependency comes with a Pull or is only suggested. */
export function DependencyPullBadge({ required, label }: { required: boolean; label: string }) {
  return required ? (
    <InfoBadge
      tone="info"
      explanation={{
        title: label,
        body: <p>The model cannot load without this Repo, so pulling the model pulls this too.</p>,
        docs: dependencyDocs,
      }}
    >
      {label}
    </InfoBadge>
  ) : (
    <InfoBadge
      explanation={{
        title: label,
        body: <p>Related, but not needed to load the model. Pull it yourself if you want it.</p>,
        docs: dependencyDocs,
      }}
    >
      {label}
    </InfoBadge>
  );
}

const roleExplanations: Record<Role, string> = {
  read: "See the Organisation's private Repos and download from them.",
  write: "Also create Repos, upload, push and start Pulls into the Organisation.",
  admin: "Also manage Members, settings and webhooks, and rename, transfer and delete Repos.",
};

/** An Organisation Member's Role, explained. */
export function RoleBadge({ role, className }: { role: Role; className?: string }) {
  return (
    <InfoBadge
      className={className}
      explanation={{
        title: `The ${role} Role`,
        body: <p>{roleExplanations[role]}</p>,
        docs: permissionsDocs,
      }}
    >
      {role}
    </InfoBadge>
  );
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

/** What each weight format is for, and whether loading it can run code. */
const formatExplanations: Record<string, string> = {
  gguf: "A single-file format for llama.cpp, Ollama and LM Studio, usually quantised. It holds only data, so loading it cannot run code.",
  safetensors:
    "Hugging Face's format for tensors. It holds only data, so loading it cannot run code. Prefer it over pickle files.",
  mlx: "Weights for Apple's MLX framework, which runs on Apple silicon.",
  onnx: "A portable model graph that ONNX Runtime and many other runtimes can run.",
  pytorch:
    "PyTorch checkpoints (.bin, .pt, .pth). They are pickles, which can run code when loaded, so Hangar scans them.",
  tensorflow: "Weights for TensorFlow or Keras.",
  flax: "Weights for Flax, the neural network library for JAX.",
  coreml: "Apple's format for running models on device with Core ML.",
  openvino: "Intel's OpenVINO format, for CPUs, GPUs and NPUs.",
  tflite: "TensorFlow Lite, for phones and small devices.",
};

export function FormatBadges({ formats }: { formats?: string[] }) {
  if (!formats?.length) return null;
  return (
    <>
      {formats.map((f) => {
        const about = formatExplanations[f.toLowerCase()];
        return about ? (
          <InfoBadge key={f} explanation={{ title: formatName(f), body: <p>{about}</p> }}>
            {formatName(f)}
          </InfoBadge>
        ) : (
          <Badge key={f}>{formatName(f)}</Badge>
        );
      })}
    </>
  );
}

/**
 * Quantisation chips, in monospace (09-web-ui.md "Visual direction"). Past
 * `max`, one "+N" chip stands for the rest and names them for screen
 * readers and in its tooltip.
 */
export function QuantChips({ quants, max = 4 }: { quants?: string[]; max?: number }) {
  if (!quants?.length) return null;
  const shown = quants.slice(0, max);
  const rest = quants.slice(max);
  const restText = `${rest.length} more ${rest.length === 1 ? "quant" : "quants"}: ${rest.join(", ")}`;
  return (
    <>
      {shown.map((q) => (
        <Badge key={q} mono>
          {q}
        </Badge>
      ))}
      {rest.length ? (
        <Badge mono role="img" aria-label={restText} title={restText}>
          +{rest.length}
        </Badge>
      ) : null}
    </>
  );
}
