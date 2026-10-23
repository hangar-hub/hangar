import type { components } from "../src/api/schema";
import { iso, type MockState } from "./data.ts";
import { findRepo, quantGroups } from "./repo-model.ts";

type S = components["schemas"];
const MiB = 1024 * 1024;
const DAY = 86_400_000;

type Upstream = {
  item: S["DiscoverItem"];
  card: string;
  files: { path: string; size: number; lfs: boolean }[];
  deps?: S["Dependency"][];
  configs?: S["DatasetConfig"][];
  preview?: Record<string, unknown>;
};

const models: Upstream[] = [
  {
    item: {
      id: "TinyLlama/TinyLlama-1.1B-Chat-GGUF",
      type: "model",
      sha: "aa11bb22cc33dd44ee55ff66aa11bb22cc33dd44",
      tags: ["gguf", "llama", "text-generation"],
      pipeline_tag: "text-generation",
      library_name: "gguf",
      downloads: 182_000,
      likes: 410,
      last_modified: iso(60 * DAY),
      arsenal: "none",
    },
    card: "# TinyLlama 1.1B Chat GGUF\n\nGGUF quantisations of TinyLlama 1.1B Chat.\n",
    files: [
      { path: "README.md", size: 2_300, lfs: false },
      { path: "tinyllama-1.1b-chat.Q2_K.gguf", size: 460 * MiB, lfs: true },
      { path: "tinyllama-1.1b-chat.Q4_K_M.gguf", size: 669 * MiB, lfs: true },
      { path: "tinyllama-1.1b-chat.Q8_0.gguf", size: 1_170 * MiB, lfs: true },
    ],
  },
  {
    item: {
      id: "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
      type: "model",
      tags: ["gguf", "qwen2", "text-generation"],
      pipeline_tag: "text-generation",
      library_name: "gguf",
      downloads: 96_400,
      likes: 220,
      last_modified: iso(20 * DAY),
      arsenal: "none",
    },
    card: "# Qwen2.5 0.5B Instruct GGUF\n\nSplit and single-file quantisations.\n",
    files: [
      { path: "README.md", size: 4_100, lfs: false },
      { path: "qwen2.5-0.5b-instruct-q4_k_m.gguf", size: 398 * MiB, lfs: true },
      { path: "qwen2.5-0.5b-instruct-q8_0.gguf", size: 676 * MiB, lfs: true },
      { path: "qwen2.5-0.5b-instruct-fp16-00001-of-00002.gguf", size: 600 * MiB, lfs: true },
      { path: "qwen2.5-0.5b-instruct-fp16-00002-of-00002.gguf", size: 348 * MiB, lfs: true },
    ],
  },
  {
    item: {
      id: "jinaai/jina-embeddings-v3",
      type: "model",
      tags: ["transformers", "safetensors", "feature-extraction", "custom_code"],
      pipeline_tag: "feature-extraction",
      library_name: "transformers",
      downloads: 1_200_000,
      likes: 900,
      last_modified: iso(90 * DAY),
      arsenal: "none",
    },
    card: "# jina-embeddings-v3\n\nMultilingual embeddings. Needs `trust_remote_code=True`.\n",
    files: [
      { path: "README.md", size: 9_000, lfs: false },
      { path: "config.json", size: 1_400, lfs: false },
      { path: "model.safetensors", size: 1_140 * MiB, lfs: true },
    ],
    deps: [
      {
        repo: "jinaai/xlm-roberta-flash-implementation",
        type: "model",
        kind: "code",
        required: true,
        source_path: "config.json",
        source_key: "auto_map.AutoModel",
        arsenal: "none",
      },
    ],
  },
  {
    item: {
      id: "ada/bert-tiny-finetune",
      type: "model",
      tags: ["transformers", "safetensors", "fill-mask"],
      pipeline_tag: "fill-mask",
      library_name: "transformers",
      downloads: 980,
      likes: 12,
      last_modified: iso(DAY),
      arsenal: "full",
      local_sha: "0123456789abcdef0123456789abcdef01234567",
    },
    card: '# bert-tiny-finetune\n\n<picture><source srcset="https://example.com/loss-dark.png"><img src="https://example.com/loss.png" alt="Loss curve"></picture>\n',
    files: [{ path: "model.safetensors", size: 17_557_844, lfs: true }],
  },
];

const datasets: Upstream[] = [
  {
    item: {
      id: "stanfordnlp/imdb",
      type: "dataset",
      tags: [
        "task_categories:text-classification",
        "modality:text",
        "format:parquet",
        "language:en",
        "size_categories:10K<n<100K",
      ],
      downloads: 210_000,
      likes: 300,
      last_modified: iso(400 * DAY),
      arsenal: "none",
    },
    card: "# IMDb\n\nLarge Movie Review Dataset: 50,000 reviews.\n",
    files: [
      { path: "plain_text/train-00000-of-00001.parquet", size: 21 * MiB, lfs: true },
      { path: "plain_text/test-00000-of-00001.parquet", size: 20 * MiB, lfs: true },
      { path: "plain_text/unsupervised-00000-of-00001.parquet", size: 42 * MiB, lfs: true },
    ],
    configs: [
      {
        config: "plain_text",
        default: true,
        splits: [
          { split: "train", status: "ready", num_rows: 25_000, num_bytes_parquet: 21 * MiB },
          { split: "test", status: "ready", num_rows: 25_000, num_bytes_parquet: 20 * MiB },
          { split: "unsupervised", status: "ready", num_rows: 50_000, num_bytes_parquet: 42 * MiB },
        ],
      },
    ],
    preview: {
      features: [
        { feature_idx: 0, name: "text", type: { dtype: "string", _type: "Value" } },
        { feature_idx: 1, name: "label", type: { names: ["neg", "pos"], _type: "ClassLabel" } },
      ],
      rows: [
        {
          row_idx: 0,
          row: { text: "I rented I AM CURIOUS-YELLOW from my video store...", label: 0 },
          truncated_cells: [],
        },
        {
          row_idx: 1,
          row: { text: "A wonderful little production.", label: 1 },
          truncated_cells: [],
        },
      ],
    },
  },
];

/** Search results with each item's Arsenal state merged in (06 "Discover"). */
export function discoverList(
  state: MockState,
  type: string,
  search: string,
  filters: string[],
): S["DiscoverItem"][] {
  const pool = type === "datasets" ? datasets : models;
  return pool
    .map((u) => withArsenal(state, u).item)
    .filter((i) => !search || i.id.toLowerCase().includes(search.toLowerCase()))
    .filter((i) =>
      filters.every((f) => i.tags?.includes(f) || i.library_name === f || i.pipeline_tag === f),
    );
}

function withArsenal(state: MockState, u: Upstream): Upstream {
  const [ns, name] = u.item.id.split("/") as [string, string];
  const local = findRepo(state, u.item.type, ns, name);
  if (!local) return u;
  const partial = (local.repo.present_bytes ?? local.repo.size_bytes) < local.repo.size_bytes;
  return { ...u, item: { ...u.item, arsenal: partial ? "partial" : "full" } };
}

export function discoverDetail(
  state: MockState,
  type: string,
  ns: string,
  name: string,
): S["DiscoverDetail"] | undefined {
  const pool = type === "datasets" ? datasets : models;
  const u = pool.find((x) => x.item.id.toLowerCase() === `${ns}/${name}`.toLowerCase());
  if (!u) return undefined;
  const merged = withArsenal(state, u);
  const local = findRepo(state, u.item.type, ns, name);
  const localFiles = new Map(
    (local?.commits[0]?.files ?? []).map((f) => [f.path, f.present !== false]),
  );
  const files = u.files.map((f) => ({ ...f, present: localFiles.get(f.path) ?? false }));
  return {
    ...merged.item,
    card: u.card,
    files,
    quants: u.item.type === "model" ? quantGroups(files) : undefined,
    configs: u.configs,
    preview: u.preview,
    dependencies: u.deps ?? [],
    total_size: files.reduce((a, f) => a + f.size, 0),
  };
}
