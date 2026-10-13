import ReactMarkdown, { type Components } from "react-markdown";
import rehypeRaw from "rehype-raw";
import rehypeSanitize, { defaultSchema } from "rehype-sanitize";
import remarkGfm from "remark-gfm";
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

/**
 * Renders user Markdown (cards, discussions) safely. External images load
 * only when the Instance allows them (`ui.external_images`).
 */
export default function Markdown({
  children,
  externalImages = true,
  resolveRelative,
}: {
  children: string;
  externalImages?: boolean;
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
        rehypePlugins={[rehypeRaw, [rehypeSanitize, cardSchema]]}
        components={components}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
