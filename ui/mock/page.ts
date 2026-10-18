/**
 * The headers and the nonce internal/web sends with UI pages, with no Node
 * APIs, so `vite preview` and the demo's Cloudflare Worker serve index.html
 * the way the Go server does.
 */

/** The Content Security Policy internal/web sets on UI pages (web.CSP). */
export function csp(nonce: string): string {
  return [
    "default-src 'self'",
    "script-src 'self'",
    `style-src 'self' 'nonce-${nonce}'`,
    "img-src 'self' data: blob: https:",
    "media-src 'self' blob:",
    "font-src 'self'",
    "connect-src 'self'",
    "worker-src 'self' blob:",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
  ].join("; ");
}

/** internal/web's PermissionsPolicy: the browser features the UI never uses. */
export const permissionsPolicy =
  "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), hid=(), bluetooth=()";

/** 128 random bits, base64 encoded, for one response (web.newNonce). */
export function newNonce(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return btoa(String.fromCharCode(...bytes));
}

/** Adds the nonce meta element at the start of the head (web.withNonce). */
export function withNonce(index: string, nonce: string): string {
  return index.replace("<head>", `<head><meta name="csp-nonce" content="${nonce}" />`);
}

/** The headers internal/web sets on index.html, for a nonce. */
export function pageHeaders(nonce: string): Record<string, string> {
  return {
    "Content-Security-Policy": csp(nonce),
    "Content-Type": "text/html; charset=utf-8",
    "Cache-Control": "no-cache",
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "strict-origin-when-cross-origin",
    "X-Frame-Options": "DENY",
    "Cross-Origin-Opener-Policy": "same-origin",
    "Permissions-Policy": permissionsPolicy,
  };
}

/**
 * True for a request internal/web answers with index.html: a GET for a page,
 * not for the API, an asset or a file.
 */
export function isUiPage(method: string, path: string, accept: string): boolean {
  const api = /^\/(api|assets|datasets-server)\//.test(path) || path.includes("/resolve/");
  return method === "GET" && accept.includes("text/html") && !api;
}
