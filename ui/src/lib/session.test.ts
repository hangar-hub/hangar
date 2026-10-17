import { afterEach, describe, expect, it, vi } from "vitest";
import { currentCsrfToken, setCsrfToken } from "@/api/client";
import { fetchMe } from "./session";

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
