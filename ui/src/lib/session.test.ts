import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { currentCsrfToken, setCsrfToken } from "@/api/client";
import { ApiError } from "./problem";
import { fetchMe, meKey, onSessionEnded } from "./session";

const me = {
  id: "00000000-0000-0000-0000-000000000001",
  username: "ada",
  display_name: "Ada",
  is_admin: false,
  orgs: [],
  csrf_token: "csrf-ada",
};

function answer(status: number, body?: unknown) {
  vi.stubGlobal("fetch", async () =>
    status === 204
      ? new Response(null, { status })
      : new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
  );
}

describe("fetchMe", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    setCsrfToken(undefined);
  });

  it("returns null when nobody is signed in (204), with no error", async () => {
    answer(204);
    await expect(fetchMe()).resolves.toBeNull();
  });

  it("returns null for a session that has ended (401)", async () => {
    setCsrfToken("stale");
    answer(401, { status: 401, title: "Unauthorized" });
    await expect(fetchMe()).resolves.toBeNull();
    expect(currentCsrfToken("")).toBeUndefined();
  });

  it("returns the user and stores the CSRF token", async () => {
    answer(200, me);
    await expect(fetchMe()).resolves.toEqual(me);
    expect(currentCsrfToken("")).toBe("csrf-ada");
  });
});

describe("onSessionEnded", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    setCsrfToken(undefined);
    window.history.replaceState(null, "", "/");
  });

  function signedInCache() {
    const qc = new QueryClient();
    qc.setQueryData(meKey, me);
    qc.setQueryData(["repos", "ada", "m"], { id: "ada/m" });
    return qc;
  }

  it("sends a user whose session ended to sign in, back to this page", async () => {
    window.history.replaceState(null, "", "/settings/tokens?sort=name");
    const qc = signedInCache();
    const toSignIn = vi.fn();
    answer(401, { status: 401, title: "Unauthorized" });
    await onSessionEnded(qc, toSignIn)(new ApiError(401));
    expect(toSignIn).toHaveBeenCalledWith("/settings/tokens?sort=name");
    expect(qc.getQueryData(meKey)).toBeNull();
    // Nothing fetched as the old user stays in the cache.
    expect(qc.getQueryData(["repos", "ada", "m"])).toBeUndefined();
  });

  it("keeps the user signed in when GET /me says the session is fine", async () => {
    const qc = signedInCache();
    const toSignIn = vi.fn();
    answer(200, me);
    await onSessionEnded(qc, toSignIn)(new ApiError(401));
    expect(toSignIn).not.toHaveBeenCalled();
    expect(qc.getQueryData(meKey)).toEqual(me);
  });

  it("leaves other errors, and signed-out visitors, to the page", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const toSignIn = vi.fn();
    await onSessionEnded(signedInCache(), toSignIn)(new ApiError(403));
    const signedOut = new QueryClient();
    signedOut.setQueryData(meKey, null);
    await onSessionEnded(signedOut, toSignIn)(new ApiError(401));
    expect(fetch).not.toHaveBeenCalled();
    expect(toSignIn).not.toHaveBeenCalled();
  });
});
