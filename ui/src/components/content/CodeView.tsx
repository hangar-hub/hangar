import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { cn } from "@/lib/utils";
import { tokenize } from "./highlight";

function useDark(): boolean {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  useEffect(() => {
    const obs = new MutationObserver(() =>
      setDark(document.documentElement.classList.contains("dark")),
    );
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    return () => obs.disconnect();
  }, []);
  return dark;
}

/** Files longer than this are shown without highlighting, to keep the page fast. */
const maxHighlight = 200_000;

/**
 * Code with line numbers, highlighted once Shiki loads. Colours are set
 * through the DOM style API, which the Content Security Policy allows,
 * never as inline style attributes in HTML.
 */
export function CodeView({
  code,
  lang,
  lineNumbers = true,
  className,
}: {
  code: string;
  lang: string;
  lineNumbers?: boolean;
  className?: string;
}) {
  const dark = useDark();
  const tokens = useQuery({
    queryKey: ["highlight", lang, code.length, code.slice(0, 200), code.slice(-200)],
    enabled: code.length <= maxHighlight,
    queryFn: () => tokenize(code, lang),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const lines = tokens.data
    ? dark
      ? tokens.data.dark
      : tokens.data.light
    : code.split("\n").map((l) => [{ content: l, color: undefined, offset: 0 }]);
  return (
    <div className={cn("overflow-x-auto rounded-lg border border-line bg-surface", className)}>
      <pre className="py-2 font-mono text-[13px] leading-5">
        <code>
          {lines.map((line, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: lines are positional
            <div key={i} className="flex min-w-max">
              {lineNumbers ? (
                <span
                  aria-hidden="true"
                  className="tabular w-12 shrink-0 select-none pr-3 text-right text-muted/70"
                >
                  {i + 1}
                </span>
              ) : null}
              <span className="pr-4 pl-1">
                {line.length === 0 || (line.length === 1 && line[0]?.content === "") ? "​" : null}
                {line.map((t, j) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: tokens are positional
                  <span key={j} style={t.color ? { color: t.color } : undefined}>
                    {t.content}
                  </span>
                ))}
              </span>
            </div>
          ))}
        </code>
      </pre>
    </div>
  );
}
