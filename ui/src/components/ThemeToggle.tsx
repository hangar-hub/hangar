import { useState } from "react";
import {
  applyTheme,
  loadTheme,
  nextTheme,
  resolveTheme,
  saveTheme,
  type ThemePreference,
} from "@/lib/theme";

const labels: Record<ThemePreference, string> = {
  system: "Theme: system",
  light: "Theme: light",
  dark: "Theme: dark",
};

/** Cycles the colour theme between system, light and dark. */
export function ThemeToggle() {
  const [pref, setPref] = useState<ThemePreference>(() => loadTheme());
  return (
    <button
      type="button"
      className="rounded-md px-2 py-1 text-sm text-neutral-600 hover:bg-neutral-100 dark:text-neutral-300 dark:hover:bg-neutral-800"
      onClick={() => {
        const next = nextTheme(pref);
        saveTheme(next);
        setPref(next);
        applyTheme(resolveTheme(next, window.matchMedia("(prefers-color-scheme: dark)").matches));
      }}
    >
      {labels[pref]}
    </button>
  );
}
