import { readFileSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { createMockCore, type MockOptions, type MockReply } from "./core.ts";
import { compileValidators, loadSpec } from "./openapi.ts";
import { isUiPage, newNonce, pageHeaders, withNonce } from "./page.ts";

/**
 * Serves the mock in core.ts from Node's HTTP server, as Connect middleware
 * for `vite` and `vite preview`.
 */

type Next = (err?: unknown) => void;

/**
 * Serves index.html for UI routes as internal/web does: a fresh style
 * nonce in a meta element and in the Content Security Policy.
 */
export function uiPages(indexPath: string) {
  return (req: IncomingMessage, res: ServerResponse, next: Next): void => {
    const path = new URL(req.url ?? "/", "http://mock.local").pathname;
    if (!isUiPage(req.method ?? "GET", path, req.headers.accept ?? "")) {
      next();
      return;
    }
    const nonce = newNonce();
    for (const [k, v] of Object.entries(pageHeaders(nonce))) res.setHeader(k, v);
    res.end(withNonce(readFileSync(indexPath, "utf8"), nonce));
  };
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

function write(res: ServerResponse, r: MockReply): void {
  res.statusCode = r.status;
  for (const [k, v] of r.headers) {
    if (k.toLowerCase() === "set-cookie") res.appendHeader(k, v);
    else res.setHeader(k, v);
  }
  res.end(r.body);
}

export function createMock(opts: MockOptions = {}) {
  const { doc, routes } = loadSpec();
  const mock = createMockCore({ routes, validators: compileValidators(doc) }, opts);

  return async function mockMiddleware(
    req: IncomingMessage,
    res: ServerResponse,
    next: Next,
  ): Promise<void> {
    const reply = await mock({
      method: req.method ?? "GET",
      url: new URL(req.url ?? "/", `http://${req.headers.host ?? "mock.local"}`),
      header: (name) => {
        const v = req.headers[name];
        return Array.isArray(v) ? v.join(", ") : v;
      },
      body: () => readBody(req),
    });
    if (reply) write(res, reply);
    else next();
  };
}
