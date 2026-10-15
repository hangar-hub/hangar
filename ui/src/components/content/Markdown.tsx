import ReactMarkdown, { type Components } from "react-markdown";
import rehypeRaw from "rehype-raw";
import rehypeSanitize, { defaultSchema } from "rehype-sanitize";
import remarkGfm from "remark-gfm";
import { ScrollRegion } from "@/components/ScrollRegion";
import { CodeView } from "./CodeView";
import { supportedLanguage } from "./highlight";

/**
 * The HTML that cards may contain: GitHub's sanitisation schema, which the
 * Hub's allowed subset follows, plus `align` on blocks and image sizes.
 * Scripts, styles, event handlers and iframes are removed.
 */
export const cardSchema = {
  ...defaultSchema,
  attributes: {
    ...defaultSchema.attributes,
    div: [...(defaultSchema.attributes?.div ?? []), "align"],
    p: [...(defaultSchema.attributes?.p ?? []), "align"],
    img: [...(defaultSchema.attributes?.img ?? []), "width", "height", "align"],
    code: [...(defaultSchema.attributes?.code ?? []), ["className", /^language-./]],
  },
};

/** True when a URL points away from this Instance. */
export function isExternal(src: string, origin: string = window.location.origin): boolean {
  try {
    return new URL(src, origin).origin !== origin;
  } catch {
    return false;
  }
}

type HastNode = { type: string; value?: string; tagName?: string; children?: HastNode[] };

function hastText(n: HastNode): string {
  return n.type === "text" ? (n.value ?? "") : (n.children ?? []).map(hastText).join("");
}

/**
 * Names a table's scroll region after its header cells, so each table on a
 * page is a distinct region: "Table: Metric, Value".
 */
export function tableLabel(table: HastNode | undefined): string {
  const headers: string[] = [];
  const walk = (n: HastNode) => {
    if (n.tagName === "th") headers.push(hastText(n).trim());
    else for (const c of n.children ?? []) walk(c);
  };
  if (table) walk(table);
  const named = headers.filter(Boolean);
  if (!named.length) return "Table";
  const label = `Table: ${named.join(", ")}`;
  return label.length > 80 ? `${label.slice(0, 79).trimEnd()}…` : label;
}

const headingTag = /^h([1-6])$/;

/**
 * A rehype plugin that moves the content's headings down the page outline:
 * its top heading level becomes `top`, and the others keep their distance
 * from it (up to h6). Markdown always sits under the page's own h1, so a
 * card's "# Title" must not become a second h1 (09-web-ui.md: one h1 per
 * page).
 */
export function nestHeadings(top: number) {
  return () => (tree: HastNode) => {
    const headings: HastNode[] = [];
    const walk = (n: HastNode) => {
      if (n.tagName && headingTag.test(n.tagName)) headings.push(n);
      for (const c of n.children ?? []) walk(c);
    };
    walk(tree);
    const levels = headings.map((h) => Number(h.tagName?.slice(1)));
    if (!levels.length) return;
    const shift = top - Math.min(...levels);
    for (const [i, h] of headings.entries()) {
      h.tagName = `h${Math.min(6, Math.max(1, (levels[i] ?? top) + shift))}`;
    }
  };
}

/**
 * Renders user Markdown (cards, discussions) safely. External images load
 * only when the Instance allows them (`ui.external_images`).
 */
export default function Markdown({
  children,
  externalImages = true,
  resolveRelative,
  headingLevel = 2,
}: {
  children: string;
  externalImages?: boolean;
  /**
   * The level the content's top heading takes: 2 under a page's h1, 3 under
   * an h2 such as a discussion's title or a dialog's.
   */
  headingLevel?: number;
  /** Turns a relative link or image path into a URL in the Repo. */
  resolveRelative?: (path: string, kind: "link" | "image") => string;
}) {
  const components: Components = {
    img({ src, alt, width, height }) {
      const url = typeof src === "string" ? src : "";
      const full =
        resolveRelative && url && !/^[a-z]+:|^\/|^#/i.test(url)
          ? resolveRelative(url, "image")
          : url;
      if (!externalImages && isExternal(full)) {
        let host = full;
        try {
          host = new URL(full).host;
        } catch {
          host = full;
        }
        return (
          <span className="inline-block rounded-sm border border-dashed border-line-strong px-2 py-1 text-xs text-muted">
            Image from {host} not loaded: this Instance does not load external images.
          </span>
        );
      }
      return <img src={full} alt={alt ?? ""} width={width} height={height} loading="lazy" />;
    },
    a({ href, children }) {
      const url = href ?? "";
      const full =
        resolveRelative && url && !/^[a-z]+:|^\/|^#/i.test(url)
          ? resolveRelative(url, "link")
          : url;
      const external = isExternal(full);
      return (
        <a
          href={full}
          {...(external ? { rel: "noopener noreferrer nofollow", target: "_blank" } : {})}
        >
          {children}
        </a>
      );
    },
    table({ node, children }) {
      // A wide table scrolls inside a region keyboard users can focus and
      // scroll, rather than pushing the page sideways.
      return (
        <ScrollRegion label={tableLabel(node as HastNode | undefined)} className="my-3">
          <table>{children}</table>
        </ScrollRegion>
      );
    },
    pre({ node }) {
      const code = node?.children.find((c) => c.type === "element" && c.tagName === "code");
      if (code?.type !== "element") return null;
      const classes = code.properties.className;
      const cls = Array.isArray(classes) ? classes.join(" ") : String(classes ?? "");
      const lang = /language-([\w-]+)/.exec(cls)?.[1];
      const text = code.children.map((c) => (c.type === "text" ? c.value : "")).join("");
      return (
        <CodeView
          code={text.replace(/\n$/, "")}
          lang={supportedLanguage(lang) ?? "text"}
          lineNumbers={false}
          className="my-3"
        />
      );
    },
  };
  return (
    <div className="prose-hangar">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeRaw, [rehypeSanitize, cardSchema], nestHeadings(headingLevel)]}
        components={components}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
