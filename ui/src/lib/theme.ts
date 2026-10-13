export type ThemePreference = "system" | "light" | "dark";

const storageKey = "hangar-theme";

/** Reads the saved preference; the default follows the system. */
export function loadTheme(
  storage: Pick<Storage, "getItem"> = window.localStorage,
): ThemePreference {
  const value = storage.getItem(storageKey);
  return value === "light" || value === "dark" ? value : "system";
}

/** Saves the preference. */
export function saveTheme(
  pref: ThemePreference,
  storage: Pick<Storage, "setItem" | "removeItem"> = window.localStorage,
): void {
  if (pref === "system") {
    storage.removeItem(storageKey);
  } else {
    storage.setItem(storageKey, pref);
  }
}

/** Resolves a preference to light or dark using the system setting. */
export function resolveTheme(pref: ThemePreference, prefersDark: boolean): "light" | "dark" {
  if (pref === "system") {
    return prefersDark ? "dark" : "light";
  }
  return pref;
}

/** Applies the theme to the document root. */
export function applyTheme(
  theme: "light" | "dark",
  root: HTMLElement = document.documentElement,
): void {
  root.classList.toggle("dark", theme === "dark");
}

/** The next preference in the toggle cycle: system, light, dark. */
export function nextTheme(pref: ThemePreference): ThemePreference {
  return pref === "system" ? "light" : pref === "light" ? "dark" : "system";
}
