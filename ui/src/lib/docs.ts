/** The public documentation site, used when the server names none. */
const publicDocs = "https://hangar.page/";

/**
 * A link to a documentation page, such as `concepts/mirrors/`, under
 * `ui.docs_url`, which the server puts in a meta element. Pages without
 * the element (the Vite dev server and tests) link to the public site.
 * Undefined when the Instance turned the links off, as an air-gapped
 * Instance without a docs mirror does.
 */
export function docsLink(path: string): string | undefined {
  const meta = document.querySelector<HTMLMetaElement>('meta[name="docs-url"]');
  const base = meta ? meta.content.trim() : publicDocs;
  if (!base) return undefined;
  return (base.endsWith("/") ? base : `${base}/`) + path;
}
