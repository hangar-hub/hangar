import { describe, expect, it } from "vitest";
import { createApiClient, setCsrfToken } from "./client";

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

    expect(seen[0]?.url).toBe("http://hangar.test/api/hangar/v1/me/tokens");
    expect(seen[0]?.headers.get("X-CSRF-Token")).toBeNull();
    expect(seen[1]?.method).toBe("POST");
    expect(seen[1]?.headers.get("X-CSRF-Token")).toBe("csrf-1");
    setCsrfToken(undefined);
  });
});
