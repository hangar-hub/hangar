import { describe, expect, it } from "vitest";
import {
  blobPath,
  encodePath,
  fileView,
  joinPath,
  languageOf,
  parseRepoId,
  repoLabel,
  repoPath,
  resolveUrl,
  treePath,
} from "./repo";

describe("repo paths", () => {
  it("labels and links Repos like the Hub", () => {
    expect(repoLabel("model", "ns", "m")).toBe("ns/m");
    expect(repoLabel("dataset", "ns", "d")).toBe("datasets/ns/d");
    expect(repoPath("dataset", "ns", "d")).toBe("/datasets/ns/d");
  });

  it("encodes Revisions with slashes as one path segment", () => {
    expect(treePath("model", "ns", "m", "refs/pr/1", "a b/c")).toBe(
      "/ns/m/tree/refs%2Fpr%2F1/a%20b/c",
    );
    expect(blobPath("model", "ns", "m", "main", "README.md")).toBe("/ns/m/blob/main/README.md");
    expect(resolveUrl("dataset", "ns", "d", "main", "data/train.csv", true)).toBe(
      "/datasets/ns/d/resolve/main/data/train.csv?download=true",
    );
    expect(encodePath("a#b/c?d")).toBe("a%23b/c%3Fd");
  });

  it("parses Repo IDs with an optional type prefix", () => {
    expect(parseRepoId("ada/bert")).toEqual({ kind: "model", ns: "ada", name: "bert" });
    expect(parseRepoId("datasets/ada/imdb")).toEqual({ kind: "dataset", ns: "ada", name: "imdb" });
    expect(parseRepoId("ada/imdb", "dataset")).toEqual({
      kind: "dataset",
      ns: "ada",
      name: "imdb",
    });
    expect(parseRepoId("just-a-name")).toBeNull();
    expect(parseRepoId("a/b/c")).toBeNull();
  });

  it("joins paths and picks viewers", () => {
    expect(joinPath("", "a/", "/b", "./c")).toBe("a/b/c");
    expect(fileView("model.safetensors")).toBe("safetensors");
    expect(fileView("x.Q4_K_M.gguf")).toBe("gguf");
    expect(fileView("README.md")).toBe("markdown");
    expect(fileView("config.json")).toBe("json");
    expect(fileView("img/cat.PNG")).toBe("image");
    expect(fileView("LICENSE")).toBe("text");
    expect(fileView("pytorch_model.bin")).toBe("binary");
    expect(languageOf("chat_template.jinja")).toBe("jinja");
    expect(languageOf("run.sh")).toBe("shellscript");
    expect(languageOf("weights.bin")).toBe("text");
  });
});
