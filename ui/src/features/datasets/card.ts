/**
 * The dataset card editor's model (DATA-13): the common fields as a form,
 * converted to and from the card's YAML metadata. Keys the form does not
 * know are kept as they are.
 */

export type ConfigForm = {
  name: string;
  isDefault: boolean;
  dataDir: string;
  splits: { split: string; path: string }[];
};

export type CardForm = {
  prettyName: string;
  licence: string;
  taskCategories: string[];
  languages: string[];
  configs: ConfigForm[];
};

export const licences = [
  "apache-2.0",
  "mit",
  "cc-by-4.0",
  "cc-by-sa-4.0",
  "cc-by-nc-4.0",
  "cc0-1.0",
  "odc-by",
  "odbl",
  "openrail",
  "llama3.1",
  "gpl-3.0",
  "other",
  "unknown",
];

export const taskCategories = [
  "text-classification",
  "token-classification",
  "question-answering",
  "summarization",
  "translation",
  "text-generation",
  "fill-mask",
  "sentence-similarity",
  "feature-extraction",
  "image-classification",
  "object-detection",
  "image-segmentation",
  "image-to-text",
  "text-to-image",
  "automatic-speech-recognition",
  "audio-classification",
  "text-to-speech",
  "tabular-classification",
  "tabular-regression",
  "time-series-forecasting",
];

function strings(v: unknown): string[] {
  if (typeof v === "string") return [v];
  if (Array.isArray(v)) return v.filter((x): x is string => typeof x === "string");
  return [];
}

function dataFiles(v: unknown): { split: string; path: string }[] {
  if (typeof v === "string") return [{ split: "train", path: v }];
  if (!Array.isArray(v)) return [];
  return v.flatMap((d) => {
    if (typeof d === "string") return [{ split: "train", path: d }];
    if (d && typeof d === "object") {
      const o = d as Record<string, unknown>;
      const paths = strings(o.path);
      return paths.map((p) => ({
        split: typeof o.split === "string" ? o.split : "train",
        path: p,
      }));
    }
    return [];
  });
}

/** Reads the form fields from card metadata. */
export function toForm(data: Record<string, unknown>): CardForm {
  const configs = Array.isArray(data.configs)
    ? data.configs
        .filter((c): c is Record<string, unknown> => Boolean(c) && typeof c === "object")
        .map((c) => ({
          name: typeof c.config_name === "string" ? c.config_name : "default",
          isDefault: c.default === true,
          dataDir: typeof c.data_dir === "string" ? c.data_dir : "",
          splits: dataFiles(c.data_files),
        }))
    : [];
  return {
    prettyName: typeof data.pretty_name === "string" ? data.pretty_name : "",
    licence: typeof data.license === "string" ? data.license : "",
    taskCategories: strings(data.task_categories),
    languages: strings(data.language),
    configs,
  };
}

/** Problems with the form that would make invalid Configs. */
export function validateForm(f: CardForm): string[] {
  const errors: string[] = [];
  const names = new Set<string>();
  f.configs.forEach((c, i) => {
    const label = c.name || `Config ${i + 1}`;
    if (!/^[A-Za-z0-9][\w.-]*$/.test(c.name))
      errors.push(`${label}: Config names use letters, digits, '-', '_' and '.'.`);
    if (names.has(c.name)) errors.push(`${label}: another Config has this name.`);
    names.add(c.name);
    if (c.splits.length === 0 && !c.dataDir)
      errors.push(`${label}: add a data folder or at least one Split with its files.`);
    c.splits.forEach((s) => {
      if (!s.split.trim() || !s.path.trim())
        errors.push(`${label}: every Split needs a name and a file path or pattern.`);
    });
  });
  if (f.configs.filter((c) => c.isDefault).length > 1)
    errors.push("Only one Config can be the default.");
  for (const l of f.languages) {
    if (!/^[a-z]{2,3}(-[A-Za-z0-9]+)*$/.test(l))
      errors.push(`${l} is not a language code such as en or pt-BR.`);
  }
  return errors;
}

/** Writes the form back into the metadata, keeping every other key. */
export function fromForm(existing: Record<string, unknown>, f: CardForm): Record<string, unknown> {
  const out: Record<string, unknown> = { ...existing };
  const set = (key: string, value: unknown) => {
    if (value === "" || value === undefined || (Array.isArray(value) && value.length === 0))
      delete out[key];
    else out[key] = value;
  };
  set("pretty_name", f.prettyName.trim());
  set("license", f.licence);
  set("task_categories", f.taskCategories);
  set("language", f.languages);
  set(
    "configs",
    f.configs.map((c) => {
      const cfg: Record<string, unknown> = { config_name: c.name };
      if (c.isDefault) cfg.default = true;
      if (c.dataDir) cfg.data_dir = c.dataDir;
      if (c.splits.length) {
        const bySplit = new Map<string, string[]>();
        for (const s of c.splits)
          bySplit.set(s.split.trim(), [...(bySplit.get(s.split.trim()) ?? []), s.path.trim()]);
        cfg.data_files = [...bySplit.entries()].map(([split, paths]) => ({
          split,
          path: paths.length === 1 ? paths[0] : paths,
        }));
      }
      return cfg;
    }),
  );
  return out;
}
