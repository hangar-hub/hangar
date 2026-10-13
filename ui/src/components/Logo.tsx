/**
 * Hangar's mark: a hangar door with the bay line across it. Decorative,
 * because every use sits next to the word Hangar.
 */
export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={className} aria-hidden="true" focusable="false">
      <path
        d="M2 21V10.5L12 4l10 6.5V21h-3.5v-7.5h-13V21z"
        fill="currentColor"
        className="text-ink"
      />
      <rect x="5.5" y="15" width="13" height="2" fill="var(--hg-accent-fill)" />
      <rect
        x="5.5"
        y="18.5"
        width="13"
        height="2.5"
        fill="currentColor"
        className="text-ink"
        opacity="0.35"
      />
    </svg>
  );
}
