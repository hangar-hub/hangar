import type { HighlighterCore, ThemedToken } from "shiki/core";

/**
 * Lazily loaded syntax highlighting. Shiki runs with its JavaScript regex
 * engine, not the WebAssembly one, because the Content Security Policy
 * does not allow WebAssembly compilation. Each language loads on first use.
 */

const languages: Record<string, () => Promise<unknown>> = {
  python: () => import("shiki/langs/python.mjs"),
  javascript: () => import("shiki/langs/javascript.mjs"),
  typescript: () => import("shiki/langs/typescript.mjs"),
  tsx: () => import("shiki/langs/tsx.mjs"),
  jsx: () => import("shiki/langs/jsx.mjs"),
  json: () => import("shiki/langs/json.mjs"),
  yaml: () => import("shiki/langs/yaml.mjs"),
  toml: () => import("shiki/langs/toml.mjs"),
  markdown: () => import("shiki/langs/markdown.mjs"),
  shellscript: () => import("shiki/langs/shellscript.mjs"),
  jinja: () => import("shiki/langs/jinja.mjs"),
  html: () => import("shiki/langs/html.mjs"),
  css: () => import("shiki/langs/css.mjs"),
  xml: () => import("shiki/langs/xml.mjs"),
  rust: () => import("shiki/langs/rust.mjs"),
  go: () => import("shiki/langs/go.mjs"),
  c: () => import("shiki/langs/c.mjs"),
  cpp: () => import("shiki/langs/cpp.mjs"),
  java: () => import("shiki/langs/java.mjs"),
  ini: () => import("shiki/langs/ini.mjs"),
  diff: () => import("shiki/langs/diff.mjs"),
};

const aliases: Record<string, string> = {
  py: "python",
  js: "javascript",
  ts: "typescript",
  sh: "shellscript",
  bash: "shellscript",
  shell: "shellscript",
  console: "shellscript",
  yml: "yaml",
  md: "markdown",
  jinja2: "jinja",
};

/** The supported language name for a fence label or file type, or undefined. */
export function supportedLanguage(lang: string | undefined): string | undefined {
  if (!lang) return undefined;
  const l = lang.toLowerCase();
  const name = aliases[l] ?? l;
  return name in languages ? name : undefined;
}

let highlighter: Promise<HighlighterCore> | undefined;

async function getHighlighter(): Promise<HighlighterCore> {
  if (!highlighter) {
    highlighter = (async () => {
      const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] = await Promise.all([
        import("shiki/core"),
        import("shiki/engine/javascript"),
      ]);
      return createHighlighterCore({
        themes: [
          import("shiki/themes/github-light-default.mjs"),
          import("shiki/themes/github-dark-default.mjs"),
        ],
        langs: [],
        engine: createJavaScriptRegexEngine(),
      });
    })();
  }
  return highlighter;
}

export type Tokens = { light: ThemedToken[][]; dark: ThemedToken[][] };

/** Tokenises code in both themes, so the view can switch without re-highlighting. */
export async function tokenize(code: string, lang: string): Promise<Tokens | null> {
  const name = supportedLanguage(lang);
  const load = name ? languages[name] : undefined;
  if (!name || !load) return null;
  const h = await getHighlighter();
  if (!h.getLoadedLanguages().includes(name)) {
    const mod = (await load()) as { default: Parameters<HighlighterCore["loadLanguage"]>[0] };
    await h.loadLanguage(mod.default);
  }
  return {
    light: h.codeToTokensBase(code, { lang: name, theme: "github-light-default" }),
    dark: h.codeToTokensBase(code, { lang: name, theme: "github-dark-default" }),
  };
}
