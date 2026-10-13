import { describe, expect, it } from "vitest";
import { applyTheme, loadTheme, nextTheme, resolveTheme, saveTheme } from "./theme";

function memoryStorage() {
  const data = new Map<string, string>();
  return {
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
    removeItem: (k: string) => void data.delete(k),
  };
}

describe("theme", () => {
  it("defaults to the system preference and round-trips choices", () => {
    const s = memoryStorage();
    expect(loadTheme(s)).toBe("system");
    saveTheme("dark", s);
    expect(loadTheme(s)).toBe("dark");
    saveTheme("system", s);
    expect(loadTheme(s)).toBe("system");
    s.setItem("hangar-theme", "purple");
    expect(loadTheme(s)).toBe("system");
  });

  it("resolves and cycles", () => {
    expect(resolveTheme("system", true)).toBe("dark");
    expect(resolveTheme("system", false)).toBe("light");
    expect(resolveTheme("light", true)).toBe("light");
    expect(nextTheme("system")).toBe("light");
    expect(nextTheme("light")).toBe("dark");
    expect(nextTheme("dark")).toBe("system");
  });

  it("applies the dark class", () => {
    const root = document.createElement("html");
    applyTheme("dark", root);
    expect(root.classList.contains("dark")).toBe(true);
    applyTheme("light", root);
    expect(root.classList.contains("dark")).toBe(false);
  });
});
