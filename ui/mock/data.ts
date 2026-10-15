import { createHash, randomUUID } from "node:crypto";
import type { components } from "../src/api/schema";

/**
 * The mock Instance's fixture data. Every browser session gets its own
 * copy, so Playwright tests running in parallel never see each other's
 * changes.
 */

type S = components["schemas"];

export type MockFile = {
  path: string;
  /** Text content, for small files. */
  text?: string;
  /** Size for LFS and binary files without text. */
  size?: number;
  lfs?: boolean;
  present?: boolean;
  scan?: "safe" | "suspicious" | "unsafe" | "error" | "pending";
  header?: S["FileHeader"];
};

export type MockCommit = {
  sha: string;
  parents: string[];
  title: string;
  message: string;
  author: string;
  date: string;
  /** Files as they are at this Commit. */
  files: MockFile[];
};

export type MockRepo = {
  repo: S["Repo"];
  commits: MockCommit[]; // newest first, on the default branch
  branches: Record<string, string>;
  tags: Record<string, string>;
  metadata?: Record<string, unknown>;
  dependencies: S["Dependencies"];
  discussions: S["Discussion"][];
  collaborators: S["Collaborator"][];
  accessRequests: S["AccessRequest"][];
  mirror?: S["Mirror"];
  configs?: S["DatasetConfigs"];
  stats: S["RepoStats"];
  /** Commits on pull request refs, which are not on the default branch. */
  prCommits?: MockCommit[];
  /** Head Commit of each pull request, by number. */
  prHeads?: Record<number, string>;
};

export type MockState = {
  signedIn: boolean;
  me: S["Me"];
  users: S["AdminUser"][];
  namespaces: S["Namespace"][];
  members: Record<string, S["Member"][]>;
  repos: MockRepo[];
  collections: S["Collection"][];
  tokens: S["Token"][];
  ollamaKeys: S["OllamaKey"][];
  upstreamTokens: Record<string, string>;
  sessions: S["Session"][];
  jobs: S["Job"][];
  upstreams: S["Upstream"][];
  settings: S["InstanceSettings"];
  webhooks: S["Webhook"][];
  deliveries: Record<string, S["WebhookDelivery"][]>;
  audit: S["AuditEvent"][];
  quotas: Record<string, number>;
  uploads: Record<
    string,
    {
      sha256: string;
      size: number;
      partSize: number;
      parts: S["UploadedPart"][];
      state: "uploading" | "verified";
      repo: string;
    }
  >;
  lfsObjects: Set<string>;
  /** Unused invite and reset link codes, as W5's Invites table holds them. */
  links: Record<string, "invite" | "reset">;
  started: number;
};

export function sha1(s: string): string {
  return createHash("sha1").update(s).digest("hex");
}

export function sha256(s: string): string {
  return createHash("sha256").update(s).digest("hex");
}

/** A UUID that is the same every run, from a name. */
export function uuidOf(name: string): string {
  const h = sha1(`uuid:${name}`);
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-7${h.slice(13, 16)}-8${h.slice(17, 20)}-${h.slice(20, 32)}`;
}

const now = Date.parse("2026-10-13T09:00:00Z");
export const iso = (msAgo: number) => new Date(now - msAgo).toISOString();
const DAY = 86_400_000;
const HOUR = 3_600_000;
const MiB = 1024 * 1024;

function safetensorsHeader(
  tensors: Record<string, { dtype: string; shape: number[] }>,
): S["FileHeader"] {
  let offset = 0;
  const header: Record<string, unknown> = { __metadata__: { format: "pt" } };
  for (const [name, t] of Object.entries(tensors)) {
    const bytes = t.shape.reduce((a, b) => a * b, 1) * (t.dtype === "F32" ? 4 : 2);
    header[name] = { dtype: t.dtype, shape: t.shape, data_offsets: [offset, offset + bytes] };
    offset += bytes;
  }
  return { format: "safetensors", header };
}

function ggufHeader(quant: string, fileType: number): S["FileHeader"] {
  return {
    format: "gguf",
    header: {
      version: 3,
      tensor_count: 201,
      metadata: {
        "general.architecture": "llama",
        "general.name": "TinyLlama 1.1B Chat v1.0",
        "general.file_type": fileType,
        "general.quantization_version": 2,
        "llama.context_length": 2048,
        "llama.embedding_length": 2048,
        "llama.block_count": 22,
        "llama.attention.head_count": 32,
        "tokenizer.ggml.model": "llama",
        "tokenizer.ggml.tokens": { type: "array", length: 32000 },
        "tokenizer.ggml.bos_token_id": 1,
        "tokenizer.ggml.eos_token_id": 2,
        "tokenizer.chat_template":
          "{% for message in messages %}<|{{ message['role'] }}|>\n{{ message['content'] }}</s>\n{% endfor %}<|assistant|>",
      },
      tensors: [
        { name: "token_embd.weight", shape: [2048, 32000], type: quant },
        { name: "blk.0.attn_q.weight", shape: [2048, 2048], type: quant },
        { name: "blk.0.attn_k.weight", shape: [2048, 256], type: quant },
        { name: "blk.0.ffn_down.weight", shape: [5632, 2048], type: "Q6_K" },
        { name: "output_norm.weight", shape: [2048], type: "F32" },
      ],
    },
  };
}

const bertConfig = JSON.stringify(
  {
    architectures: ["BertForMaskedLM"],
    model_type: "bert",
    hidden_size: 128,
    num_attention_heads: 2,
    num_hidden_layers: 2,
    max_position_embeddings: 512,
    vocab_size: 30522,
  },
  null,
  2,
);

const bertCard = `---
license: apache-2.0
language: en
library_name: transformers
pipeline_tag: fill-mask
tags:
  - bert
  - tiny
datasets:
  - ada/imdb-mini
---

# bert-tiny-finetune

A two-layer BERT fine-tuned on **imdb-mini** for quick experiments.

## Use it

\`\`\`python
from transformers import AutoModelForMaskedLM
model = AutoModelForMaskedLM.from_pretrained("ada/bert-tiny-finetune")
\`\`\`

| Metric | Value |
|---|---|
| Accuracy | 0.81 |
| Parameters | 4.4M |

<script>alert("cards never run scripts")</script>

![Loss curve](https://example.com/loss.png)
`;

const llamaCard = `---
license: apache-2.0
base_model: TinyLlama/TinyLlama-1.1B-Chat-v1.0
pipeline_tag: text-generation
tags:
  - gguf
---

# TinyLlama 1.1B Chat GGUF

GGUF quantisations of TinyLlama 1.1B Chat, for llama.cpp, Ollama and LM Studio.

| Quant | Size | Use |
|---|---|---|
| Q2_K | 460 MiB | Smallest, lowest quality |
| Q4_K_M | 638 MiB | Recommended |
| Q8_0 | 1.1 GiB | Near lossless |
`;

const imdbCard = `---
pretty_name: IMDb mini
license: other
task_categories:
  - text-classification
language:
  - en
size_categories:
  - 1K<n<10K
configs:
  - config_name: default
    data_files:
      - split: train
        path: data/train.csv
      - split: test
        path: data/test.csv
---

# IMDb mini

2,500 film reviews labelled positive or negative, cut down from IMDb for tests.
`;

function csv(rows: number, offset: number): string {
  const lines = ["text,label"];
  for (let i = 0; i < Math.min(rows, 20); i++) {
    lines.push(
      `"Review ${offset + i}: ${i % 2 ? "a quiet triumph" : "two hours I will not get back"}",${i % 2}`,
    );
  }
  return `${lines.join("\n")}\n`;
}

const remoteConfig = JSON.stringify(
  {
    architectures: ["QwenCoderForCausalLM"],
    auto_map: {
      AutoConfig: "ml-team/qwen-modeling-code--configuration_qwen.QwenConfig",
      AutoModelForCausalLM: "ml-team/qwen-modeling-code--modeling_qwen.QwenCoderForCausalLM",
    },
    max_position_embeddings: 32768,
  },
  null,
  2,
);

function makeCommits(
  id: string,
  history: { title: string; author: string; ago: number; files: MockFile[] }[],
): MockCommit[] {
  const out: MockCommit[] = [];
  let parent: string | undefined;
  for (const h of history) {
    const sha = sha1(`${id}:${h.title}`);
    out.unshift({
      sha,
      parents: parent ? [parent] : [],
      title: h.title,
      message: h.title,
      author: h.author,
      date: iso(h.ago),
      files: h.files,
    });
    parent = sha;
  }
  return out;
}

function stats(base: number): S["RepoStats"] {
  const days = Array.from({ length: 30 }, (_, i) => ({
    day: new Date(now - (29 - i) * DAY).toISOString().slice(0, 10),
    count: Math.round(base * (0.6 + ((i * 37) % 11) / 10)),
  }));
  return { days, total: days.reduce((a, d) => a + d.count, 0) };
}

function repoBase(
  type: "model" | "dataset",
  namespace: string,
  name: string,
  extra: Partial<S["Repo"]>,
): S["Repo"] {
  return {
    id: `${namespace}/${name}`,
    uuid: uuidOf(`${type}/${namespace}/${name}`),
    type,
    namespace,
    name,
    visibility: "public",
    default_branch: "main",
    mirror: false,
    gated: "",
    created_at: iso(40 * DAY),
    updated_at: iso(2 * DAY),
    last_modified: iso(2 * DAY),
    likes: 3,
    liked: false,
    downloads: 120,
    size_bytes: 0,
    lfs_bytes: 0,
    tags: [],
    permissions: { read: true, write: true, admin: true, discuss: true, pull_mirror: true },
    ...extra,
  };
}

function sizeOf(files: MockFile[]): number {
  return files.reduce((a, f) => a + (f.size ?? Buffer.byteLength(f.text ?? "")), 0);
}

function finishRepo(
  r: Omit<MockRepo, "branches" | "tags" | "stats"> & { tags?: Record<string, string> },
): MockRepo {
  const head = r.commits[0] as MockCommit;
  const size = sizeOf(head.files);
  const present = head.files.reduce(
    (a, f) => a + (f.present === false ? 0 : (f.size ?? Buffer.byteLength(f.text ?? ""))),
    0,
  );
  r.repo.sha = head.sha;
  r.repo.size_bytes = size;
  r.repo.lfs_bytes = head.files.filter((f) => f.lfs).reduce((a, f) => a + (f.size ?? 0), 0);
  r.repo.present_bytes = present;
  return {
    ...r,
    branches: { main: head.sha },
    tags: r.tags ?? {},
    stats: stats(r.repo.downloads / 30),
  };
}

function llamaRepo(): MockRepo {
  const files: MockFile[] = [
    { path: ".gitattributes", text: "*.gguf filter=lfs diff=lfs merge=lfs -text\n" },
    { path: "README.md", text: llamaCard },
    {
      path: "tinyllama-1.1b-chat.Q4_K_M.gguf",
      size: 669 * MiB,
      lfs: true,
      present: true,
      scan: "safe",
      header: ggufHeader("Q4_K", 15),
    },
    {
      path: "tinyllama-1.1b-chat.Q8_0.gguf",
      size: 1_170 * MiB,
      lfs: true,
      present: false,
      header: ggufHeader("Q8_0", 7),
    },
    {
      path: "tinyllama-1.1b-chat.Q2_K.gguf",
      size: 460 * MiB,
      lfs: true,
      present: false,
      header: ggufHeader("Q2_K", 10),
    },
  ];
  const id = "TinyLlama/TinyLlama-1.1B-Chat-GGUF";
  return finishRepo({
    repo: repoBase("model", "TinyLlama", "TinyLlama-1.1B-Chat-GGUF", {
      mirror: true,
      mirror_status: {
        upstream: "huggingface",
        upstream_repo: id,
        upstream_url: `https://huggingface.co/${id}`,
        status: "partial",
        last_synced_at: iso(3 * HOUR),
        update_available: false,
      },
      likes: 41,
      downloads: 2310,
      licence: "apache-2.0",
      tags: ["gguf", "llama", "text-generation"],
      permissions: { read: true, write: true, admin: true, discuss: true, pull_mirror: true },
      summary: {
        formats: ["gguf"],
        architecture: "llama",
        parameters: 1_100_048_384,
        quants: ["Q2_K", "Q4_K_M", "Q8_0"],
        context_length: 2048,
        pipeline_tag: "text-generation",
        library_name: "gguf",
        scan_status: "safe",
      },
    }),
    commits: makeCommits(id, [
      { title: "Initial commit", author: "TheBloke", ago: 90 * DAY, files: files.slice(0, 2) },
      { title: "Upload GGUF quantisations", author: "TheBloke", ago: 60 * DAY, files },
    ]),
    metadata: {
      formats: ["gguf"],
      architecture: "llama",
      parameters: { total: 1_100_048_384 },
      quantisations: [
        { file: "tinyllama-1.1b-chat.Q4_K_M.gguf", quant: "Q4_K_M", file_type: 15 },
        { file: "tinyllama-1.1b-chat.Q8_0.gguf", quant: "Q8_0", file_type: 7 },
        { file: "tinyllama-1.1b-chat.Q2_K.gguf", quant: "Q2_K", file_type: 10 },
      ],
      context_length: 2048,
      chat_template: "{% for message in messages %}...",
      licence: "apache-2.0",
      pipeline_tag: "text-generation",
      library_name: "gguf",
      card_data: { license: "apache-2.0", base_model: "TinyLlama/TinyLlama-1.1B-Chat-v1.0" },
    },
    dependencies: {
      needs: [
        {
          repo: "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
          type: "model",
          kind: "card_base_model",
          required: false,
          source_path: "README.md",
          source_key: "base_model",
          arsenal: "none",
        },
      ],
      needed_by: [],
    },
    discussions: [],
    collaborators: [],
    accessRequests: [],
    mirror: {
      upstream: "huggingface",
      upstream_repo: id,
      upstream_url: `https://huggingface.co/${id}`,
      status: "partial",
      last_synced_at: iso(3 * HOUR),
      update_available: false,
      selection: { quants: ["Q4_K_M"] },
      track: true,
      track_ref: "main",
      schedule: "0 */6 * * *",
      last_synced_commit: sha1(`${id}:Upload GGUF quantisations`),
    },
  });
}

function bertRepo(): MockRepo {
  const id = "ada/bert-tiny-finetune";
  const v1: MockFile[] = [
    {
      path: ".gitattributes",
      text: "*.safetensors filter=lfs diff=lfs merge=lfs -text\n*.bin filter=lfs diff=lfs merge=lfs -text\n",
    },
    { path: "README.md", text: bertCard.replace("0.81", "0.78") },
    { path: "config.json", text: bertConfig },
    { path: "vocab.txt", text: "[PAD]\n[UNK]\n[CLS]\n[SEP]\n[MASK]\nthe\nof\nand\n" },
  ];
  const v2: MockFile[] = [
    ...v1.filter((f) => f.path !== "README.md"),
    { path: "README.md", text: bertCard },
    {
      path: "model.safetensors",
      size: 17_557_844,
      lfs: true,
      present: true,
      scan: "safe",
      header: safetensorsHeader({
        "bert.embeddings.word_embeddings.weight": { dtype: "F32", shape: [30522, 128] },
        "bert.embeddings.position_embeddings.weight": { dtype: "F32", shape: [512, 128] },
        "bert.encoder.layer.0.attention.self.query.weight": { dtype: "F32", shape: [128, 128] },
        "bert.encoder.layer.0.attention.self.query.bias": { dtype: "F32", shape: [128] },
        "cls.predictions.bias": { dtype: "F32", shape: [30522] },
      }),
    },
    {
      path: "tokenizer_config.json",
      text: JSON.stringify({ do_lower_case: true, model_max_length: 512 }, null, 2),
    },
    { path: "onnx/model.onnx", size: 17_900_000, lfs: true, present: true },
  ];
  return finishRepo({
    repo: repoBase("model", "ada", "bert-tiny-finetune", {
      likes: 7,
      downloads: 412,
      licence: "apache-2.0",
      description: "A two-layer BERT for quick experiments.",
      tags: ["bert", "tiny"],
      summary: {
        formats: ["safetensors", "onnx"],
        architecture: "BertForMaskedLM",
        parameters: 4_386_178,
        context_length: 512,
        pipeline_tag: "fill-mask",
        library_name: "transformers",
        scan_status: "safe",
      },
    }),
    commits: makeCommits(id, [
      { title: "Initial commit", author: "ada", ago: 30 * DAY, files: v1 },
      { title: "Add safetensors weights and ONNX export", author: "ada", ago: 2 * DAY, files: v2 },
    ]),
    tags: { "v1.0": sha1(`${id}:Initial commit`) },
    metadata: {
      formats: ["safetensors", "onnx"],
      architecture: "BertForMaskedLM",
      parameters: { total: 4_386_178, F32: 4_386_178 },
      context_length: 512,
      licence: "apache-2.0",
      pipeline_tag: "fill-mask",
      library_name: "transformers",
      card_data: { license: "apache-2.0", language: "en", datasets: ["ada/imdb-mini"] },
    },
    dependencies: {
      needs: [
        {
          repo: "ada/imdb-mini",
          type: "dataset",
          kind: "dataset",
          required: false,
          source_path: "README.md",
          source_key: "datasets",
          arsenal: "full",
        },
      ],
      needed_by: [],
    },
    discussions: [
      {
        num: 1,
        kind: "discussion",
        title: "Which tokenizer settings did you use?",
        status: "open",
        author: "grace",
        created_at: iso(5 * DAY),
        events: [
          {
            id: uuidOf("d1e1"),
            type: "comment",
            author: "grace",
            body: "Is `model_max_length` 512 on purpose?",
            created_at: iso(5 * DAY),
          },
          {
            id: uuidOf("d1e2"),
            type: "comment",
            author: "ada",
            body: "Yes, it matches the position embeddings.",
            created_at: iso(4 * DAY),
          },
        ],
      },
      {
        num: 2,
        kind: "pull_request",
        title: "Add an ONNX export",
        status: "open",
        author: "grace",
        pr_ref: "refs/pr/2",
        pr_base: "main",
        created_at: iso(3 * DAY),
        events: [
          {
            id: uuidOf("d2e1"),
            type: "comment",
            author: "grace",
            body: "Exported with `optimum-cli export onnx`.",
            created_at: iso(3 * DAY),
          },
        ],
        changes: [
          {
            path: "onnx/model.onnx",
            status: "added",
            new_size: 17_900_000,
            lfs: true,
            binary: true,
            new_oid: sha256("onnx"),
          },
        ],
      },
    ],
    collaborators: [{ username: "grace", role: "write" }],
    accessRequests: [],
  });
}

function remoteRepo(): MockRepo {
  const id = "ml-team/qwen-coder-remote";
  const files: MockFile[] = [
    {
      path: "README.md",
      text: "# qwen-coder-remote\n\nA coding model that needs `trust_remote_code=True`.\n",
    },
    { path: "config.json", text: remoteConfig },
    { path: "model.safetensors", size: 52 * MiB, lfs: true, present: true, scan: "safe" },
  ];
  return finishRepo({
    repo: repoBase("model", "ml-team", "qwen-coder-remote", {
      visibility: "private",
      likes: 2,
      downloads: 30,
      summary: {
        formats: ["safetensors"],
        architecture: "QwenCoderForCausalLM",
        parameters: 26_000_000,
        context_length: 32768,
        pipeline_tag: "text-generation",
        library_name: "transformers",
        scan_status: "safe",
      },
    }),
    commits: makeCommits(id, [{ title: "Initial commit", author: "ada", ago: 8 * DAY, files }]),
    metadata: {
      formats: ["safetensors"],
      parameters: { total: 26_000_000 },
      library_name: "transformers",
    },
    dependencies: {
      needs: [
        {
          repo: "ml-team/qwen-modeling-code",
          type: "model",
          kind: "code",
          required: true,
          source_path: "config.json",
          source_key: "auto_map.AutoModelForCausalLM",
          arsenal: "full",
        },
      ],
      needed_by: [],
    },
    discussions: [],
    collaborators: [],
    accessRequests: [],
  });
}

function legacyRepo(): MockRepo {
  const id = "ada/legacy-checkpoint";
  const files: MockFile[] = [
    { path: "README.md", text: "# legacy-checkpoint\n\nAn old PyTorch Lightning checkpoint.\n" },
    { path: "model.ckpt", size: 8 * MiB, lfs: true, present: true, scan: "unsafe" },
    { path: "optimizer.pt", size: 2 * MiB, lfs: true, present: true, scan: "suspicious" },
  ];
  return finishRepo({
    repo: repoBase("model", "ada", "legacy-checkpoint", {
      visibility: "internal",
      gated: "manual",
      likes: 0,
      downloads: 4,
      summary: { formats: ["pytorch"], scan_status: "unsafe" },
    }),
    commits: makeCommits(id, [
      { title: "Upload checkpoint", author: "ada", ago: 120 * DAY, files },
    ]),
    metadata: { formats: ["pytorch"] },
    dependencies: { needs: [], needed_by: [] },
    discussions: [],
    collaborators: [],
    accessRequests: [
      {
        id: uuidOf("ar1"),
        username: "grace",
        status: "pending",
        fields: { reason: "Reproducing the 2025 results" },
        created_at: iso(DAY),
      },
    ],
  });
}

function imdbRepo(): MockRepo {
  const id = "ada/imdb-mini";
  const files: MockFile[] = [
    { path: ".gitattributes", text: "*.parquet filter=lfs diff=lfs merge=lfs -text\n" },
    { path: "README.md", text: imdbCard },
    { path: "data/train.csv", text: csv(2000, 0), size: 1_820_000 },
    { path: "data/test.csv", text: csv(500, 2000), size: 455_000 },
  ];
  const commits = makeCommits(id, [
    { title: "Add train and test splits", author: "ada", ago: 6 * DAY, files },
  ]);
  return finishRepo({
    repo: repoBase("dataset", "ada", "imdb-mini", {
      likes: 5,
      downloads: 77,
      licence: "other",
      tags: ["text-classification", "en"],
      summary: { formats: ["csv"], pipeline_tag: "text-classification" },
    }),
    commits,
    metadata: {
      formats: ["csv"],
      card_data: {
        pretty_name: "IMDb mini",
        license: "other",
        language: ["en"],
        task_categories: ["text-classification"],
      },
      modalities: ["text"],
      size_category: "1K<n<10K",
      trained_models: ["ada/bert-tiny-finetune"],
    },
    dependencies: {
      needs: [],
      needed_by: [
        {
          repo: "ada/bert-tiny-finetune",
          type: "model",
          kind: "dataset",
          required: false,
          source_path: "README.md",
          source_key: "datasets",
          arsenal: "full",
        },
      ],
    },
    discussions: [],
    collaborators: [],
    accessRequests: [],
    configs: {
      commit: (commits[0] as MockCommit).sha,
      from_card: true,
      configs: [
        {
          config: "default",
          default: true,
          splits: [
            {
              split: "train",
              status: "ready",
              num_rows: 2000,
              num_bytes_source: 1_820_000,
              num_bytes_parquet: 1_102_331,
            },
            {
              split: "test",
              status: "ready",
              num_rows: 500,
              num_bytes_source: 455_000,
              num_bytes_parquet: 281_090,
            },
          ],
        },
      ],
    },
  });
}

function cifarRepo(): MockRepo {
  const id = "ada/cifar-mini";
  const files: MockFile[] = [
    {
      path: "README.md",
      text: "---\npretty_name: CIFAR mini\nlicense: mit\ntask_categories:\n  - image-classification\n---\n\n# CIFAR mini\n\n300 small images in 3 classes.\n",
    },
    { path: "train/metadata.csv", text: "file_name,label\n0.png,cat\n" },
    { path: "train/0.png", size: 2048 },
  ];
  const commits = makeCommits(id, [{ title: "Add images", author: "ada", ago: 12 * DAY, files }]);
  return finishRepo({
    repo: repoBase("dataset", "ada", "cifar-mini", {
      likes: 1,
      downloads: 12,
      licence: "mit",
      summary: { formats: ["imagefolder"] },
    }),
    commits,
    metadata: {
      modalities: ["image"],
      size_category: "n<1K",
      card_data: { pretty_name: "CIFAR mini", license: "mit" },
    },
    dependencies: { needs: [], needed_by: [] },
    discussions: [],
    collaborators: [],
    accessRequests: [],
    configs: {
      commit: (commits[0] as MockCommit).sha,
      from_card: false,
      configs: [
        {
          config: "default",
          default: true,
          splits: [
            {
              split: "train",
              status: "ready",
              num_rows: 300,
              num_bytes_source: 640_000,
              num_bytes_parquet: 610_000,
            },
          ],
        },
      ],
    },
  });
}

function speechRepo(): MockRepo {
  const id = "ml-team/speech-commands-mini";
  const files: MockFile[] = [
    { path: "README.md", text: "# Speech commands mini\n\nOne-second clips of spoken commands.\n" },
    { path: "data/train-00000-of-00001.tar", size: 48 * MiB, lfs: true, present: true },
  ];
  const commits = makeCommits(id, [
    { title: "Upload WebDataset shards", author: "ada", ago: HOUR, files },
  ]);
  return finishRepo({
    repo: repoBase("dataset", "ml-team", "speech-commands-mini", {
      visibility: "internal",
      likes: 0,
      downloads: 0,
      summary: { formats: ["webdataset"] },
    }),
    commits,
    metadata: { modalities: ["audio"] },
    dependencies: { needs: [], needed_by: [] },
    discussions: [],
    collaborators: [],
    accessRequests: [],
    configs: {
      commit: (commits[0] as MockCommit).sha,
      from_card: false,
      configs: [
        {
          config: "default",
          default: true,
          splits: [
            {
              split: "train",
              status: "pending",
              num_bytes_source: 48 * MiB,
              job_id: uuidOf("job-convert"),
            },
          ],
        },
      ],
    },
  });
}

export function createState(): MockState {
  const repos = [
    bertRepo(),
    llamaRepo(),
    remoteRepo(),
    legacyRepo(),
    imdbRepo(),
    cifarRepo(),
    speechRepo(),
  ];
  const me: S["Me"] = {
    id: uuidOf("user-ada"),
    username: "ada",
    display_name: "Ada Lovelace",
    email: "ada@example.com",
    is_admin: true,
    orgs: [{ name: "ml-team", display_name: "ML team", role: "admin" }],
    csrf_token: "mock-csrf-token",
  };
  const pullJob: S["Job"] = {
    id: uuidOf("job-pull-q8"),
    kind: "pull",
    status: "running",
    attempts: 1,
    max_attempts: 5,
    payload: { repo: "TinyLlama/TinyLlama-1.1B-Chat-GGUF", upstream: "huggingface", type: "model" },
    progress: {
      bytes_done: 412 * MiB,
      bytes_total: 1_170 * MiB,
      files_done: 0,
      files_total: 1,
      rate: 4.8 * MiB,
      eta_seconds: 158,
      files: [
        {
          path: "tinyllama-1.1b-chat.Q8_0.gguf",
          bytes_done: 412 * MiB,
          bytes_total: 1_170 * MiB,
          status: "running",
        },
      ],
    },
    created_by: "ada",
    created_at: iso(10 * 60_000),
    started_at: iso(9 * 60_000),
  };
  return {
    signedIn: false,
    me,
    users: [
      {
        id: me.id,
        username: "ada",
        display_name: "Ada Lovelace",
        email: "ada@example.com",
        is_admin: true,
        disabled: false,
        created_at: iso(200 * DAY),
        last_login_at: iso(HOUR),
      },
      {
        id: uuidOf("user-grace"),
        username: "grace",
        display_name: "Grace Hopper",
        email: "grace@example.com",
        is_admin: false,
        disabled: false,
        oidc_providers: ["unifi"],
        created_at: iso(100 * DAY),
        last_login_at: iso(DAY),
      },
      {
        id: uuidOf("user-alan"),
        username: "alan",
        display_name: "Alan Turing",
        is_admin: false,
        disabled: true,
        created_at: iso(150 * DAY),
      },
    ],
    namespaces: [
      {
        name: "ada",
        kind: "user",
        display_name: "Ada Lovelace",
        description: "Small models for quick experiments.",
        created_at: iso(200 * DAY),
      },
      { name: "grace", kind: "user", display_name: "Grace Hopper", created_at: iso(100 * DAY) },
      {
        name: "ml-team",
        kind: "org",
        display_name: "ML team",
        description: "Shared fine-tunes and datasets.",
        member_count: 2,
        created_at: iso(90 * DAY),
      },
      {
        name: "TinyLlama",
        kind: "org",
        display_name: "TinyLlama",
        reserved_mirror: true,
        created_at: iso(60 * DAY),
      },
    ],
    members: {
      "ml-team": [
        { username: "ada", display_name: "Ada Lovelace", role: "admin", source: "manual" },
        { username: "grace", display_name: "Grace Hopper", role: "write", source: "oidc" },
      ],
    },
    repos,
    collections: [
      {
        namespace: "ada",
        slug: "small-and-fast",
        title: "Small and fast",
        description: "Models that run on a laptop.",
        visibility: "public",
        updated_at: iso(DAY),
        items: [
          {
            id: uuidOf("ci1"),
            type: "model",
            item_id: "TinyLlama/TinyLlama-1.1B-Chat-GGUF",
            position: 0,
            note: "Q4_K_M is the sweet spot.",
          },
          {
            id: uuidOf("ci2"),
            type: "model",
            item_id: "ada/bert-tiny-finetune",
            position: 1,
          },
          { id: uuidOf("ci3"), type: "dataset", item_id: "ada/imdb-mini", position: 2 },
        ],
      },
    ],
    tokens: [
      {
        id: uuidOf("tok-laptop"),
        name: "laptop",
        prefix: "hgr_4xQp",
        scope: "write",
        created_at: iso(20 * DAY),
        last_used_at: iso(HOUR),
      },
    ],
    ollamaKeys: [],
    upstreamTokens: {},
    sessions: [
      {
        id: uuidOf("sess-1"),
        user_agent: "Mozilla/5.0 (Macintosh)",
        ip: "192.0.2.10",
        created_at: iso(DAY),
        last_seen_at: iso(60_000),
        expires_at: iso(-29 * DAY),
        current: true,
      },
      {
        id: uuidOf("sess-2"),
        user_agent: "Mozilla/5.0 (iPhone)",
        ip: "192.0.2.44",
        created_at: iso(6 * DAY),
        last_seen_at: iso(2 * DAY),
        expires_at: iso(-24 * DAY),
        current: false,
      },
    ],
    jobs: [
      pullJob,
      {
        id: uuidOf("job-index"),
        kind: "index",
        status: "succeeded",
        attempts: 1,
        max_attempts: 3,
        payload: { repo: "ada/bert-tiny-finetune" },
        progress: { bytes_done: 0, bytes_total: 0, files_done: 7, files_total: 7, rate: 0 },
        created_by: "ada",
        created_at: iso(2 * DAY),
        finished_at: iso(2 * DAY - 30_000),
      },
      {
        id: uuidOf("job-convert"),
        kind: "dataset.convert",
        status: "pending",
        attempts: 0,
        max_attempts: 3,
        payload: { repo: "ml-team/speech-commands-mini" },
        progress: {
          bytes_done: 0,
          bytes_total: 48 * MiB,
          files_done: 0,
          files_total: 1,
          rate: 0,
          message: "Waiting for a worker",
        },
        created_by: "ada",
        created_at: iso(HOUR),
      },
      {
        id: uuidOf("job-failed"),
        kind: "pull",
        status: "failed",
        attempts: 5,
        max_attempts: 5,
        payload: { repo: "acme/huge-model" },
        progress: { bytes_done: 0, bytes_total: 0, files_done: 0, files_total: 0, rate: 0 },
        error:
          "The Upstream answered 401: the Repo is gated. Save a token for huggingface.co in Settings and retry.",
        created_by: "grace",
        created_at: iso(3 * DAY),
        finished_at: iso(3 * DAY - 60_000),
      },
    ],
    upstreams: [
      {
        id: uuidOf("up-hf"),
        name: "huggingface",
        display_name: "huggingface.co",
        base_url: "https://huggingface.co",
        enabled: true,
        source: "config",
        has_instance_token: false,
      },
      {
        id: uuidOf("up-corp"),
        name: "corp",
        display_name: "Corporate mirror",
        base_url: "https://hub.corp.example",
        enabled: false,
        source: "admin",
        has_instance_token: true,
        namespace_prefix: "corp-",
      },
    ],
    settings: {
      anonymous_read: false,
      registration: "invite",
      local_login: true,
      ui_external_images: true,
      scan_block_unsafe_downloads: false,
      webhooks_allow_private_targets: true,
      pull_bandwidth_limit: 0,
      pull_concurrency: 4,
      pull_windows: [],
      pull_allow_override_windows: true,
      pull_through_enabled: false,
      pull_through_allow: ["*"],
      pull_through_min_role: "user",
      oidc_providers: [{ id: "unifi", name: "UniFi", issuer: "https://sso.example.com" }],
    },
    webhooks: [
      {
        id: uuidOf("wh1"),
        namespace: "ada",
        url: "https://ci.example.com/hooks/hangar",
        events: ["repo.updated", "pull.completed"],
        enabled: true,
        created_at: iso(10 * DAY),
        last_delivery: {
          id: uuidOf("whd1"),
          event: "repo.updated",
          status_code: 200,
          attempt: 1,
          duration_ms: 84,
          created_at: iso(2 * DAY),
          delivered_at: iso(2 * DAY),
        },
      },
    ],
    deliveries: {
      [uuidOf("wh1")]: [
        {
          id: uuidOf("whd1"),
          event: "repo.updated",
          status_code: 200,
          attempt: 1,
          duration_ms: 84,
          created_at: iso(2 * DAY),
          delivered_at: iso(2 * DAY),
          payload: '{"event":"repo.updated"}',
        },
        {
          id: uuidOf("whd2"),
          event: "pull.completed",
          status_code: 502,
          attempt: 2,
          duration_ms: 10_000,
          error: "Timed out after 10 seconds",
          next_attempt_at: iso(-25 * 60_000),
          created_at: iso(DAY),
          payload: '{"event":"pull.completed"}',
        },
      ],
    },
    audit: [
      {
        id: "a1",
        action: "auth.login",
        actor: "ada",
        target_type: "user",
        target_id: "ada",
        ip: "192.0.2.10",
        created_at: iso(HOUR),
      },
      {
        id: "a2",
        action: "token.created",
        actor: "ada",
        target_type: "token",
        target_id: "laptop",
        ip: "192.0.2.10",
        created_at: iso(20 * DAY),
      },
      {
        id: "a3",
        action: "repo.visibility_changed",
        actor: "ada",
        target_type: "repo",
        target_id: "models/ml-team/qwen-coder-remote",
        data: { from: "public", to: "private" },
        created_at: iso(5 * DAY),
      },
      {
        id: "a4",
        action: "user.disabled",
        actor: "ada",
        target_type: "user",
        target_id: "alan",
        created_at: iso(30 * DAY),
      },
    ],
    quotas: { "ml-team": 200 * 1024 * MiB },
    uploads: {},
    lfsObjects: new Set(),
    links: {},
    started: Date.now(),
  };
}

export { randomUUID };
