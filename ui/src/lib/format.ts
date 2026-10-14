/** Formats a byte count with binary units, as the Hub and git-lfs do. */
export function formatBytes(bytes: number | null | undefined, digits = 1): string {
  if (bytes === null || bytes === undefined || Number.isNaN(bytes)) return "Unknown";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(value >= 100 ? 0 : digits)} ${units[unit]}`;
}

/** Formats a parameter count the way model names do: 125M, 1.1B, 70B. */
export function formatParams(n: number | null | undefined): string {
  if (n === null || n === undefined) return "Unknown";
  const steps: [number, string][] = [
    [1e12, "T"],
    [1e9, "B"],
    [1e6, "M"],
    [1e3, "K"],
  ];
  for (const [size, suffix] of steps) {
    if (n >= size) {
      const v = n / size;
      return `${v >= 100 ? v.toFixed(0) : Number(v.toFixed(1))}${suffix}`;
    }
  }
  return String(n);
}

/** Formats a count with grouping, in British English. */
export function formatCount(n: number | null | undefined): string {
  if (n === null || n === undefined) return "Unknown";
  return new Intl.NumberFormat("en-GB").format(n);
}

/** Formats a compact count such as 12.3k for downloads and likes. */
export function formatCompact(n: number | null | undefined): string {
  if (n === null || n === undefined) return "Unknown";
  return new Intl.NumberFormat("en-GB", { notation: "compact", maximumFractionDigits: 1 })
    .format(n)
    .toLowerCase();
}

/** Formats a context length such as 4096 as 4K, or 131072 as 128K. */
export function formatContext(n: number | null | undefined): string {
  if (n === null || n === undefined) return "Unknown";
  if (n >= 1024 && n % 1024 === 0) return `${n / 1024}K`;
  return formatCount(n);
}

/** Formats a date as 13 October 2026. */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "Unknown";
  return new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "long",
    year: "numeric",
  }).format(d);
}

/** Formats a date and time with the time zone, as 13 Oct 2026, 14:05 UTC. */
export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "Unknown";
  return new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  }).format(d);
}

/** Formats how long ago a time was: "3 minutes ago", "yesterday". */
export function relativeTime(iso: string | null | undefined, now: Date = new Date()): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "Unknown";
  const seconds = Math.round((d.getTime() - now.getTime()) / 1000);
  const rtf = new Intl.RelativeTimeFormat("en-GB", { numeric: "auto" });
  const abs = Math.abs(seconds);
  if (abs < 45) return "just now";
  if (abs < 3600) return rtf.format(Math.round(seconds / 60), "minute");
  if (abs < 86400) return rtf.format(Math.round(seconds / 3600), "hour");
  if (abs < 86400 * 30) return rtf.format(Math.round(seconds / 86400), "day");
  if (abs < 86400 * 365) return rtf.format(Math.round(seconds / (86400 * 30)), "month");
  return rtf.format(Math.round(seconds / (86400 * 365)), "year");
}

/** Formats a transfer rate in bytes per second. */
export function formatRate(bytesPerSecond: number | null | undefined): string {
  if (!bytesPerSecond) return "Unknown";
  return `${formatBytes(bytesPerSecond)}/s`;
}

/** Formats a duration in seconds as 1 h 5 min, 4 min 10 s or 12 s. */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || seconds < 0) return "Unknown";
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = s % 60;
  if (h > 0) return `${h} h ${m} min`;
  if (m > 0) return `${m} min ${r} s`;
  return `${r} s`;
}

/** The first 7 characters of a Commit SHA, as git shows it. */
export function shortSha(sha: string | null | undefined): string {
  return sha ? sha.slice(0, 7) : "";
}

/** A percentage from 0 to 100, safe when the total is zero. */
export function percent(done: number, total: number): number {
  if (!total || total <= 0) return 0;
  return Math.min(100, Math.max(0, (done / total) * 100));
}
