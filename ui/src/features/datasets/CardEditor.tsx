import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { useMemo, useState } from "react";
import { stringify } from "yaml";
import { api } from "@/api/client";
import type { Repo } from "@/api/types";
import { CodeView } from "@/components/content/CodeView";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/form";
import { InlineError } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { joinFrontMatter } from "@/lib/frontmatter";
import { unwrap } from "@/lib/problem";
import { repoKey, repoPathParams } from "../repos/hooks";
import {
  type CardForm,
  type ConfigForm,
  fromForm,
  licences,
  taskCategories,
  toForm,
  validateForm,
} from "./card";

function toBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

/**
 * The dataset card editor (DATA-13): a form for pretty name, licence, task
 * categories, languages and Configs that writes valid YAML front matter
 * into README.md, committed to the default branch.
 */
export function CardEditor({
  repo,
  data,
  body,
  parentCommit,
  onDone,
}: {
  repo: Repo;
  data: Record<string, unknown>;
  body: string;
  parentCommit?: string;
  onDone: () => void;
}) {
  const [form, setForm] = useState<CardForm>(() => toForm(data));
  const [text, setText] = useState(body);
  const [summary, setSummary] = useState("Update dataset card");
  const toast = useToast();
  const qc = useQueryClient();
  const ref = { kind: repo.type, ns: repo.namespace, name: repo.name };
  const errors = validateForm(form);
  const merged = useMemo(() => fromForm(data, form), [data, form]);
  const yaml = useMemo(() => stringify(merged, { lineWidth: 0 }).trimEnd(), [merged]);

  const save = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/repos/{type}/{ns}/{name}/commits", {
          params: { path: repoPathParams(ref) },
          body: {
            summary,
            parent_commit: parentCommit,
            branch: repo.default_branch,
            operations: [
              {
                op: "add",
                path: "README.md",
                content_base64: toBase64(joinFrontMatter(merged, text)),
              },
            ],
          },
        }),
      ),
    onSuccess: () => {
      toast("Dataset card saved.");
      qc.invalidateQueries({ queryKey: repoKey(ref) });
      onDone();
    },
  });

  const update = (patch: Partial<CardForm>) => setForm((f) => ({ ...f, ...patch }));
  const updateConfig = (i: number, patch: Partial<ConfigForm>) =>
    update({ configs: form.configs.map((c, j) => (j === i ? { ...c, ...patch } : c)) });

  return (
    <form
      className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,0.8fr)]"
      onSubmit={(e) => {
        e.preventDefault();
        if (errors.length === 0) save.mutate();
      }}
    >
      <div className="space-y-5">
        <Field label="Pretty name" hint="A readable title, such as IMDb mini.">
          {(p) => (
            <Input
              {...p}
              value={form.prettyName}
              onChange={(e) => update({ prettyName: e.target.value })}
            />
          )}
        </Field>
        <Field label="Licence">
          {(p) => (
            <Select
              {...p}
              value={form.licence}
              onChange={(e) => update({ licence: e.target.value })}
            >
              <option value="">Not set</option>
              {[...new Set([form.licence, ...licences].filter(Boolean))].map((l) => (
                <option key={l} value={l}>
                  {l}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <fieldset>
          <legend className="text-sm font-medium">Task categories</legend>
          <div className="mt-2 flex flex-wrap gap-1.5">
            {[...new Set([...taskCategories, ...form.taskCategories])].map((t) => {
              const on = form.taskCategories.includes(t);
              return (
                <button
                  key={t}
                  type="button"
                  aria-pressed={on}
                  onClick={() =>
                    update({
                      taskCategories: on
                        ? form.taskCategories.filter((x) => x !== t)
                        : [...form.taskCategories, t],
                    })
                  }
                  className={`rounded-sm border px-2 py-0.5 text-xs ${on ? "border-accent/40 bg-accent-soft font-medium text-accent" : "border-line text-muted hover:bg-raised"}`}
                >
                  {t}
                </button>
              );
            })}
          </div>
        </fieldset>
        <Field label="Languages" hint="Language codes separated by commas, such as en, fr, pt-BR.">
          {(p) => (
            <Input
              {...p}
              value={form.languages.join(", ")}
              onChange={(e) =>
                update({
                  languages: e.target.value
                    .split(",")
                    .map((s) => s.trim())
                    .filter(Boolean),
                })
              }
            />
          )}
        </Field>
        <fieldset className="space-y-3">
          <legend className="text-sm font-medium">Configs</legend>
          <p className="text-xs text-muted">
            Each Config names its Splits and the files in them. Without Configs, Hangar infers
            Splits from file and folder names.
          </p>
          {form.configs.map((c, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: Configs are edited by position
            <div key={i} className="space-y-3 rounded-lg border border-line p-3">
              <div className="flex flex-wrap items-end gap-3">
                <Field label="Config name" className="min-w-40 flex-1">
                  {(p) => (
                    <Input
                      {...p}
                      value={c.name}
                      onChange={(e) => updateConfig(i, { name: e.target.value })}
                    />
                  )}
                </Field>
                <Field label="Data folder" className="min-w-40 flex-1">
                  {(p) => (
                    <Input
                      {...p}
                      value={c.dataDir}
                      placeholder="Optional"
                      onChange={(e) => updateConfig(i, { dataDir: e.target.value })}
                    />
                  )}
                </Field>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={`Remove Config ${c.name || i + 1}`}
                  onClick={() => update({ configs: form.configs.filter((_, j) => j !== i) })}
                >
                  <Trash2 aria-hidden="true" />
                </Button>
              </div>
              <Checkbox
                label="Default Config"
                checked={c.isDefault}
                onChange={(e) => updateConfig(i, { isDefault: e.target.checked })}
              />
              {c.splits.map((s, k) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: Splits are edited by position
                <div key={k} className="flex flex-wrap items-end gap-2">
                  <Field label="Split" className="w-32">
                    {(p) => (
                      <Input
                        {...p}
                        value={s.split}
                        onChange={(e) =>
                          updateConfig(i, {
                            splits: c.splits.map((x, m) =>
                              m === k ? { ...x, split: e.target.value } : x,
                            ),
                          })
                        }
                      />
                    )}
                  </Field>
                  <Field label="Files" className="min-w-48 flex-1">
                    {(p) => (
                      <Input
                        {...p}
                        value={s.path}
                        placeholder="data/train-*.parquet"
                        className="font-mono"
                        onChange={(e) =>
                          updateConfig(i, {
                            splits: c.splits.map((x, m) =>
                              m === k ? { ...x, path: e.target.value } : x,
                            ),
                          })
                        }
                      />
                    )}
                  </Field>
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`Remove Split ${s.split || k + 1}`}
                    onClick={() => updateConfig(i, { splits: c.splits.filter((_, m) => m !== k) })}
                  >
                    <Trash2 aria-hidden="true" />
                  </Button>
                </div>
              ))}
              <Button
                size="sm"
                onClick={() =>
                  updateConfig(i, {
                    splits: [...c.splits, { split: c.splits.length ? "test" : "train", path: "" }],
                  })
                }
              >
                <Plus aria-hidden="true" />
                Add a Split
              </Button>
            </div>
          ))}
          <Button
            size="sm"
            onClick={() =>
              update({
                configs: [
                  ...form.configs,
                  {
                    name: form.configs.length ? "" : "default",
                    isDefault: form.configs.length === 0,
                    dataDir: "",
                    splits: [{ split: "train", path: "" }],
                  },
                ],
              })
            }
          >
            <Plus aria-hidden="true" />
            Add a Config
          </Button>
        </fieldset>
        <Field label="Card text" hint="Markdown, shown below the metadata.">
          {(p) => (
            <Textarea
              {...p}
              rows={10}
              className="font-mono text-[13px]"
              value={text}
              onChange={(e) => setText(e.target.value)}
            />
          )}
        </Field>
      </div>
      <div className="space-y-4">
        <div>
          <h2 className="mb-2 text-sm font-medium">YAML metadata</h2>
          <CodeView code={yaml || "# No metadata"} lang="yaml" lineNumbers={false} />
        </div>
        {errors.length ? (
          <ul
            className="space-y-1 rounded-md border border-danger/30 bg-danger-soft p-3 text-sm text-danger"
            aria-label="Problems to fix"
          >
            {errors.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        ) : null}
        <Field label="Commit message">
          {(p) => (
            <Input {...p} value={summary} onChange={(e) => setSummary(e.target.value)} required />
          )}
        </Field>
        <InlineError error={save.error} />
        <div className="flex gap-2">
          <Button
            type="submit"
            variant="primary"
            disabled={errors.length > 0 || save.isPending || !summary.trim()}
          >
            Commit changes
          </Button>
          <Button onClick={onDone}>Cancel</Button>
        </div>
      </div>
    </form>
  );
}
