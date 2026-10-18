import { Info } from "lucide-react";

/** The documentation site the demo links to. */
export const docsUrl = "https://hangar.page/";

/**
 * True in the demo build at demo.hangar.page (VITE_HANGAR_DEMO=1). Vite
 * replaces the flag at build time, so the embedded build leaves the banner
 * out entirely.
 */
export const isDemo = import.meta.env.VITE_HANGAR_DEMO === "1";

/** Tells visitors to the demo that its data is sample data and is not kept. */
export function DemoBanner() {
  return (
    <section
      aria-label="Demo"
      className="border-b border-line bg-info-soft px-4 py-2 text-center text-sm text-ink"
    >
      <Info aria-hidden="true" className="mr-1.5 inline size-4 align-[-3px] text-info" />
      This is a demo with sample data. Nothing you do here is saved.{" "}
      <a href={docsUrl} className="font-medium underline underline-offset-2">
        Read the documentation
      </a>
    </section>
  );
}
