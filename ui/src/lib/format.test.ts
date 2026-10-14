import { describe, expect, it } from "vitest";
import {
  formatBytes,
  formatCompact,
  formatContext,
  formatDate,
  formatDuration,
  formatParams,
  percent,
  relativeTime,
  shortSha,
} from "./format";

describe("format", () => {
  it("formats sizes in binary units", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1023)).toBe("1023 B");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(669 * 1024 * 1024)).toBe("669 MiB");
    expect(formatBytes(5 * 1024 ** 4)).toBe("5.0 TiB");
    expect(formatBytes(undefined)).toBe("Unknown");
  });

  it("formats parameter counts like model names", () => {
    expect(formatParams(4_386_178)).toBe("4.4M");
    expect(formatParams(1_100_048_384)).toBe("1.1B");
    expect(formatParams(70_000_000_000)).toBe("70B");
    expect(formatParams(125_000_000)).toBe("125M");
    expect(formatParams(999)).toBe("999");
  });

  it("formats context lengths and compact counts", () => {
    expect(formatContext(4096)).toBe("4K");
    expect(formatContext(131072)).toBe("128K");
    expect(formatContext(1000)).toBe("1,000");
    expect(formatCompact(182_000)).toBe("182k");
    expect(formatCompact(1_200_000)).toBe("1.2m");
  });

  it("formats dates in British English", () => {
    expect(formatDate("2026-10-13T09:00:00Z")).toBe("13 October 2026");
    expect(formatDate("not a date")).toBe("Unknown");
  });

  it("formats relative times", () => {
    const now = new Date("2026-10-13T12:00:00Z");
    expect(relativeTime("2026-10-13T11:59:50Z", now)).toBe("just now");
    expect(relativeTime("2026-10-13T11:55:00Z", now)).toBe("5 minutes ago");
    expect(relativeTime("2026-10-12T12:00:00Z", now)).toBe("yesterday");
    expect(relativeTime("2026-07-13T12:00:00Z", now)).toBe("3 months ago");
  });

  it("formats durations, SHAs and percentages", () => {
    expect(formatDuration(12)).toBe("12 s");
    expect(formatDuration(250)).toBe("4 min 10 s");
    expect(formatDuration(3900)).toBe("1 h 5 min");
    expect(shortSha("0123456789abcdef")).toBe("0123456");
    expect(percent(5, 10)).toBe(50);
    expect(percent(5, 0)).toBe(0);
    expect(percent(20, 10)).toBe(100);
  });
});
