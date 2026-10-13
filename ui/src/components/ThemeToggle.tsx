import { Monitor, Moon, Sun } from "lucide-react";
import { useState } from "react";
import { Button } from "@/components/ui/button";
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

const icons: Record<ThemePreference, typeof Sun> = { system: Monitor, light: Sun, dark: Moon };

/** Cycles the colour theme between system, light and dark. */
export function ThemeToggle() {
  const [pref, setPref] = useState<ThemePreference>(() => loadTheme());
  const Icon = icons[pref];
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label={labels[pref]}
      title={labels[pref]}
      onClick={() => {
        const next = nextTheme(pref);
        saveTheme(next);
        setPref(next);
        applyTheme(resolveTheme(next, window.matchMedia("(prefers-color-scheme: dark)").matches));
      }}
    >
      <Icon aria-hidden="true" />
    </Button>
  );
}
