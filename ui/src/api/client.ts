import createClient, { type Middleware } from "openapi-fetch";
import type { paths } from "./schema";

/** Base path of the native API. */
export const apiBase = "/api/hangar/v1";

let csrfToken: string | undefined;

/** Stores the CSRF token from GET /me; state-changing calls send it. */
export function setCsrfToken(token: string | undefined): void {
  csrfToken = token;
}

const safeMethods = new Set(["GET", "HEAD", "OPTIONS"]);

/** Adds the X-CSRF-Token header to state-changing requests (05-auth.md "Sessions"). */
export const csrfMiddleware: Middleware = {
  onRequest({ request }) {
    if (csrfToken && !safeMethods.has(request.method.toUpperCase())) {
      request.headers.set("X-CSRF-Token", csrfToken);
    }
    return request;
  },
};

/** Creates a typed client for the native API. */
export function createApiClient(options: { baseUrl?: string; fetch?: typeof fetch } = {}) {
  const client = createClient<paths>({
    baseUrl: options.baseUrl ?? apiBase,
    fetch: options.fetch,
    credentials: "same-origin",
  });
  client.use(csrfMiddleware);
  return client;
}

/** The shared client the UI uses. */
export const api = createApiClient();
