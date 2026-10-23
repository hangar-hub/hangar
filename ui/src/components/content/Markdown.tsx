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

const whitespace = /\s/;

/**
 * Rewrites a `srcset` list, following the HTML parsing rules: `map` gets
 * each candidate's URL and returns the URL to use, or null to drop the
 * candidate. Descriptors such as `2x` and `640w` are kept.
 */
export function rewriteSrcset(srcset: string, map: (url: string) => string | null): string {
  const out: string[] = [];
  const n = srcset.length;
  let i = 0;
  while (i < n) {
    while (i < n && (whitespace.test(srcset.charAt(i)) || srcset.charAt(i) === ",")) i++;
    if (i >= n) break;
    let url = "";
    while (i < n && !whitespace.test(srcset.charAt(i))) url += srcset.charAt(i++);
    let descriptors = "";
    if (url.endsWith(",")) {
      url = url.replace(/,+$/, "");
    } else {
      // Descriptors run to the next comma outside parentheses.
      let depth = 0;
      for (; i < n; i++) {
        const c = srcset.charAt(i);
        if (c === "," && depth === 0) break;
        if (c === "(") depth++;
        else if (c === ")") depth = Math.max(0, depth - 1);
        descriptors += c;
      }
      i++;
    }
    const mapped = url ? map(url) : null;
    if (mapped) out.push([mapped, descriptors.trim()].filter(Boolean).join(" "));
  }
  return out.join(", ");
}

/** The host of a URL, for a message, or the URL itself when it does not parse. */
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
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
 * only when the Instance allows them (`ui.external_images`), through `img`
 * and through `srcset` on a `picture`'s `source`: the only attributes the
 * sanitiser keeps that make the browser fetch something.
 */
export default function Markdown({
  children,
  externalImages,
  resolveRelative,
  headingLevel = 2,
}: {
  children: string;
  /**
   * True when the Instance allows images from other hosts. False, or
   * undefined while the setting is not known, blocks them.
   */
  externalImages?: boolean;
  /**
   * The level the content's top heading takes: 2 under a page's h1, 3 under
   * an h2 such as a discussion's title or a dialog's.
   */
  headingLevel?: number;
  /** Turns a relative link or image path into a URL in the Repo. */
  resolveRelative?: (path: string, kind: "link" | "image") => string;
}) {
  // Blocks an image URL on another host, or one that does not parse, unless
  // the Instance allows external images.
  const blocked = (url: string) => {
    if (externalImages === true) return false;
    try {
      return new URL(url, window.location.origin).origin !== window.location.origin;
    } catch {
      return true;
    }
  };
  const imageUrl = (url: string) =>
    resolveRelative && url && !/^[a-z]+:|^\/|^#/i.test(url) ? resolveRelative(url, "image") : url;
  const components: Components = {
    img({ src, alt, width, height }) {
      const full = imageUrl(typeof src === "string" ? src : "");
      if (blocked(full)) {
        return (
          <span className="inline-block rounded-sm border border-dashed border-line-strong px-2 py-1 text-xs text-muted">
            {externalImages === false
              ? `Image from ${hostOf(full)} not loaded: this Instance does not load external images.`
              : `Image from ${hostOf(full)} not loaded.`}
          </span>
        );
      }
      return <img src={full} alt={alt ?? ""} width={width} height={height} loading="lazy" />;
    },
    source({ srcSet, media, type, width, height }) {
      // A picture's source loads its srcset instead of the img inside it,
      // so it follows the same rules as img: relative paths point into the
      // Repo, and other hosts are dropped unless the Instance allows them.
      const list = rewriteSrcset(typeof srcSet === "string" ? srcSet : "", (url) => {
        const full = imageUrl(url);
        return blocked(full) ? null : full;
      });
      if (!list) return null;
      return <source srcSet={list} media={media} type={type} width={width} height={height} />;
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
