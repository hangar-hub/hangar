import { describe, expect, it } from "vitest";
import { auditQuery, dayBound } from "./admin/AdminAuditPage";
import { bandwidthBytes, parseWindows } from "./admin/AdminPullsPage";
import { quotaBytes } from "./admin/AdminStoragePage";
import { oidcErrors, safeRedirect } from "./auth/LoginPage";
import { validRepoName } from "./repos/NewRepoPage";
import { normaliseUserCode } from "./settings/DevicePage";
import { expiryFrom } from "./settings/TokensPage";

describe("form helpers", () => {
  it("only follows same-origin redirects after sign-in", () => {
    expect(safeRedirect("/ada/m")).toBe("/ada/m");
    expect(safeRedirect("//evil.example")).toBe("/");
    expect(safeRedirect("https://evil.example")).toBe("/");
    expect(safeRedirect(undefined)).toBe("/");
    expect(oidcErrors.oidc_link_required).toMatch(/link the provider from Settings/);
  });

  it("validates Repo names as the Hub does", () => {
    expect(validRepoName("bert-tiny_v1.5")).toBe(true);
    expect(validRepoName("-bad")).toBe(false);
    expect(validRepoName("a..b")).toBe(false);
    expect(validRepoName("x.git")).toBe(false);
    expect(validRepoName("a/b")).toBe(false);
  });

  it("computes Token expiries", () => {
    expect(expiryFrom("30", new Date("2026-10-13T00:00:00Z"))).toBe("2026-11-12T00:00:00.000Z");
    expect(expiryFrom("")).toBeUndefined();
  });

  it("formats device codes", () => {
    expect(normaliseUserCode("abcd1234")).toBe("ABCD-1234");
    expect(normaliseUserCode("ab-c")).toBe("ABC");
    expect(normaliseUserCode("abcd-12345678")).toBe("ABCD-1234");
  });

  it("parses Pull windows, bandwidth and quotas", () => {
    expect(parseWindows("01:00-07:00\n 22:30-23:59 ")).toEqual({
      windows: ["01:00-07:00", "22:30-23:59"],
      invalid: [],
    });
    expect(parseWindows("1:00-7:00, 25:00-26:00").invalid).toEqual(["1:00-7:00", "25:00-26:00"]);
    expect(bandwidthBytes("2.5")).toBe(2_621_440);
    expect(bandwidthBytes("")).toBe(0);
    expect(quotaBytes("1")).toBe(1024 ** 3);
    expect(quotaBytes("0")).toBe(0);
  });

  it("bounds audit dates to whole UTC days", () => {
    expect(dayBound("2026-10-01", false)).toBe("2026-10-01T00:00:00Z");
    expect(dayBound("2026-10-01", true)).toBe("2026-10-01T23:59:59Z");
    expect(dayBound("01/10/2026", true)).toBeUndefined();
    expect(auditQuery({ actor: "ada", since: "2026-10-01" })).toEqual({
      actor: "ada",
      since: "2026-10-01T00:00:00Z",
    });
  });
});
