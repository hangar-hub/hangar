// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// The accent is Taxiway teal, decided by James on 2026-10-16
// (docs/build/decisions.md). Warnings keep their own amber.
const src = fileURLToPath(new URL(".", import.meta.url));
const css = readFileSync(join(src, "styles.css"), "utf8");

function block(selector: string): string {
  const start = css.indexOf(`${selector} {`);
  expect(start, `${selector} in styles.css`).toBeGreaterThan(-1);
  return css.slice(start, css.indexOf("}", start));
}

function token(scope: string, name: string): string | undefined {
  return new RegExp(`--${name}:\\s*([^;]+);`).exec(scope)?.[1]?.trim();
}

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    if (statSync(p).isDirectory()) return sources(p);
    return /\.tsx?$/.test(f) && !/\.test\.tsx?$/.test(f) ? [p] : [];
  });
}

describe("palette", () => {
  it("uses Taxiway teal for the accent in light and dark", () => {
    const light = block(":root");
    const dark = block(":root.dark");
    expect({
      accent: token(light, "hg-accent"),
      fill: token(light, "hg-accent-fill"),
      ink: token(light, "hg-accent-ink"),
      soft: token(light, "hg-accent-soft"),
    }).toEqual({ accent: "#0b6e64", fill: "#0b7a6e", ink: "#ffffff", soft: "#dff3ef" });
    expect({
      accent: token(dark, "hg-accent"),
      fill: token(dark, "hg-accent-fill"),
      ink: token(dark, "hg-accent-ink"),
      soft: token(dark, "hg-accent-soft"),
    }).toEqual({ accent: "#5fd4c2", fill: "#2bb3a0", ink: "#021a17", soft: "#12332e" });
  });

  it("keeps warnings amber, apart from the accent", () => {
    expect(token(block(":root"), "hg-warn")).toBe("#7a4a00");
    expect(token(block(":root.dark"), "hg-warn")).toBe("#f5c46b");
  });

  it("has no amber colour tokens, and no component picks amber directly", () => {
    expect(css).not.toMatch(/--color-amber-/);
    const offenders = sources(src).filter((p) => /\bamber-\d/.test(readFileSync(p, "utf8")));
    expect(offenders).toEqual([]);
  });
});
