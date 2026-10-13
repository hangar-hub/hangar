import { describe, expect, it } from "vitest";
import { createApiClient, currentCsrfToken, setCsrfToken } from "./client";

describe("api client", () => {
  it("calls the native API and sends the CSRF token only on state changes", async () => {
    const seen: Request[] = [];
    const fakeFetch = async (input: Request) => {
      seen.push(input);
      return new Response(JSON.stringify({ items: [] }), {
        headers: { "Content-Type": "application/json" },
      });
    };
    const client = createApiClient({
      baseUrl: "http://hangar.test/api/hangar/v1",
      fetch: fakeFetch as typeof fetch,
    });
    setCsrfToken("csrf-1");

    const list = await client.GET("/me/tokens");
    expect(list.data).toEqual({ items: [] });
    await client.POST("/me/tokens", { body: { name: "laptop", scope: "write" } });
    await client.GET("/repos/{type}/{ns}/{name}/tree/{rev}/{path}", {
      params: { path: { type: "models", ns: "ns", name: "m", rev: "refs/pr/1", path: "a/b.txt" } },
    });

    expect(seen[0]?.url).toBe("http://hangar.test/api/hangar/v1/me/tokens");
    expect(seen[0]?.headers.get("X-CSRF-Token")).toBeNull();
    expect(seen[1]?.method).toBe("POST");
    expect(seen[1]?.headers.get("X-CSRF-Token")).toBe("csrf-1");
    // Revisions and paths are one percent-encoded segment each, as the API expects.
    expect(seen[2]?.url).toBe(
      "http://hangar.test/api/hangar/v1/repos/models/ns/m/tree/refs%2Fpr%2F1/a%2Fb.txt",
    );
    setCsrfToken(undefined);
  });

  it("falls back to the readable hangar_csrf cookie", () => {
    setCsrfToken(undefined);
    expect(currentCsrfToken("a=1; hangar_csrf=abc%3D; b=2")).toBe("abc=");
    expect(currentCsrfToken("a=1")).toBeUndefined();
    setCsrfToken("from-me");
    expect(currentCsrfToken("hangar_csrf=cookie")).toBe("from-me");
    setCsrfToken(undefined);
  });
});
