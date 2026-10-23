// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { repoIdQuery } from "./discover/DiscoverPage";
import { homeExamples } from "./HomePage";
import { scopeHelp } from "./settings/TokensPage";

// UI copy that the phase 3 docs walk-through found out of step with the
// product or the docs (docs/build/docs-walkthrough.md).
const docs = fileURLToPath(new URL("../../../docs/", import.meta.url));
const doc = (path: string) => readFileSync(join(docs, path), "utf8");

describe("UI copy agrees with the docs", () => {
  it("names Repos the docs use in the landing page's example commands", () => {
    // TinyLlama/TinyLlama-1.1B-Chat-GGUF, the old example, does not exist on
    // huggingface.co, so the commands could never work.
    expect(doc("guides/vllm.md")).toContain(homeExamples.download);
    expect(doc("guides/ollama.md")).toContain(`${homeExamples.ollama}:Q4_K_M`);
  });

  it("describes Token scopes as the permissions page does", () => {
    // A write Token administers Repos the user is an admin of only through
    // the Hub API calls huggingface_hub makes (decided by James on
    // 2026-10-17, "Hub actions only"); the admin scope covers every other
    // Repo administration, Organisation Members and the Instance.
    expect(scopeHelp.write).toMatch(/start Pulls/);
    expect(scopeHelp.write).toMatch(/huggingface_hub/);
    expect(scopeHelp.write).toMatch(/Repos you administer/);
    expect(scopeHelp.write).not.toMatch(/webhooks|transfer/);
    expect(scopeHelp.admin).toMatch(/webhooks/);
    expect(scopeHelp.admin).toMatch(/Organisation Members/);
    const permissions = doc("concepts/permissions.md");
    expect(permissions).toMatch(/through `huggingface_hub`/);
    expect(permissions).toMatch(/\| `admin` \| Also everything else that administers a Repo/);
  });
});

describe("repoIdQuery", () => {
  // Discover offers to open a Repo by ID, because huggingface.co search
  // leaves some organisations out, such as hf-internal-testing, whose
  // tiny-random-bert the first-pull guide uses.
  it("reads a full Repo ID", () => {
    expect(repoIdQuery("hf-internal-testing/tiny-random-bert")).toBe(
      "hf-internal-testing/tiny-random-bert",
    );
    expect(repoIdQuery("  Qwen/Qwen3-0.6B ")).toBe("Qwen/Qwen3-0.6B");
    expect(repoIdQuery("lhoestq/demo1")).toBe("lhoestq/demo1");
  });

  it("leaves other searches alone", () => {
    expect(repoIdQuery(undefined)).toBeUndefined();
    expect(repoIdQuery("")).toBeUndefined();
    expect(repoIdQuery("tiny-random-bert")).toBeUndefined();
    expect(repoIdQuery("llama gguf")).toBeUndefined();
    expect(repoIdQuery("a/b/c")).toBeUndefined();
    expect(repoIdQuery("/name")).toBeUndefined();
    expect(repoIdQuery("ns/")).toBeUndefined();
  });

  it("is what the first-pull guide tells people to search for", () => {
    expect(doc("get-started/first-pull.md")).toContain("hf-internal-testing/tiny-random-bert");
  });
});
