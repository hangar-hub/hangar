import createClient, { type Middleware } from "openapi-fetch";
import type { paths } from "./schema";

/** Base path of the native API. */
export const apiBase = "/api/hangar/v1";

let csrfToken: string | undefined;

/** Stores the CSRF token from GET /me; state-changing calls send it. */
export function setCsrfToken(token: string | undefined): void {
  csrfToken = token;
}

/**
 * The CSRF token to send: the one from GET /me, or the readable
 * `hangar_csrf` cookie the server sets with the session (05-auth.md).
 */
export function currentCsrfToken(
  cookie: string = globalThis.document?.cookie ?? "",
): string | undefined {
  if (csrfToken) return csrfToken;
  const match = cookie.split(/;\s*/).find((c) => c.startsWith("hangar_csrf="));
  return match ? decodeURIComponent(match.slice("hangar_csrf=".length)) : undefined;
}

const safeMethods = new Set(["GET", "HEAD", "OPTIONS"]);

/** Adds the X-CSRF-Token header to state-changing requests (05-auth.md "Sessions"). */
export const csrfMiddleware: Middleware = {
  onRequest({ request }) {
    const token = currentCsrfToken();
    if (token && !safeMethods.has(request.method.toUpperCase())) {
      request.headers.set("X-CSRF-Token", token);
    }
    return request;
  },
};

/** Creates a typed client for the native API. */
export function createApiClient(options: { baseUrl?: string; fetch?: typeof fetch } = {}) {
  const client = createClient<paths>({
    baseUrl: options.baseUrl ?? apiBase,
    // Resolve fetch on each call rather than once, so tests can replace it.
    fetch: options.fetch ?? ((input: Request, init?: RequestInit) => globalThis.fetch(input, init)),
    credentials: "same-origin",
  });
  client.use(csrfMiddleware);
  return client;
}

/**
 * The shared client the UI uses, with an absolute base URL so requests are
 * built the same way in the browser and in tests.
 */
export const api = createApiClient({
  baseUrl: typeof window === "undefined" ? apiBase : new URL(apiBase, window.location.origin).href,
});

/**
 * Sends a same-origin request outside the generated client (the `/auth/`
 * handler, the Dataset Viewer API and upload parts), with the CSRF header
 * on state-changing calls.
 */
export function rawFetch(input: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const method = (init.method ?? "GET").toUpperCase();
  const token = currentCsrfToken();
  const sameOrigin = input.startsWith("/");
  if (token && sameOrigin && !safeMethods.has(method)) headers.set("X-CSRF-Token", token);
  return fetch(input, { ...init, headers, credentials: sameOrigin ? "same-origin" : "omit" });
}
