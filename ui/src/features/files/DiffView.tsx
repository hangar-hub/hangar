import { ScrollRegion } from "@/components/ScrollRegion";
import { cn } from "@/lib/utils";

/** The kind of each line in a unified diff. */
export function diffLines(
  patch: string,
): { kind: "add" | "del" | "hunk" | "meta" | "ctx"; text: string }[] {
  return patch.split("\n").map((text) => {
    if (text.startsWith("+++") || text.startsWith("---")) return { kind: "meta", text };
    if (text.startsWith("@@")) return { kind: "hunk", text };
    if (text.startsWith("+")) return { kind: "add", text };
    if (text.startsWith("-")) return { kind: "del", text };
    return { kind: "ctx", text };
  });
}

/**
 * A unified diff. Added and removed lines keep their + and - signs, so
 * the colour is never the only signal. Give each diff on a page its own
 * label, because each is a landmark region.
 */
export function DiffView({ patch, label = "Diff" }: { patch: string; label?: string }) {
  return (
    <ScrollRegion label={label} className="overflow-y-hidden">
      <pre className="font-mono text-[12px] leading-5">
        <code>
          {diffLines(patch).map((l, i) => (
            <div
              // biome-ignore lint/suspicious/noArrayIndexKey: diff lines are positional
              key={i}
              className={cn(
                "min-w-max px-3",
                l.kind === "add" && "bg-ok-soft text-ok",
                l.kind === "del" && "bg-danger-soft text-danger",
                l.kind === "hunk" && "bg-info-soft text-info",
                l.kind === "meta" && "text-muted",
              )}
            >
              {l.text || "​"}
            </div>
          ))}
        </code>
      </pre>
    </ScrollRegion>
  );
}
