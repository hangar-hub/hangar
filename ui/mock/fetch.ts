import type { MockHandler } from "./core.ts";
import { problem } from "./handlers.ts";

/**
 * Serves the mock in core.ts through the Fetch API, as a Cloudflare Worker
 * or a Pages Function does: it takes a Request and returns a Response, or
 * null when the mock does not handle the request.
 */

export type FetchMockOptions = {
  /**
   * The largest request body, in bytes, the adapter reads. A larger one
   * gets 413 before the mock sees it, so nobody can use the demo to store
   * files.
   */
  maxBodyBytes: number;
};

/** Reads a request body up to limit bytes; null when it is longer. */
export async function readCapped(
  body: ReadableStream<Uint8Array>,
  limit: number,
): Promise<Buffer | null> {
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > limit) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

function tooLarge(limit: number): Response {
  const size = limit >= 1024 * 1024 ? `${limit / 1024 / 1024} MiB` : `${limit / 1024} KiB`;
  const p = problem(413, `The demo accepts request bodies up to ${size}. Try a smaller file.`);
  return new Response(JSON.stringify(p.body), {
    status: 413,
    headers: { "Content-Type": "application/problem+json" },
  });
}

export function createFetchMock(mock: MockHandler, opts: FetchMockOptions) {
  return async function fetchMock(request: Request): Promise<Response | null> {
    const limit = opts.maxBodyBytes;
    const declared = Number(request.headers.get("content-length") ?? 0);
    let raw: Buffer = Buffer.alloc(0);
    if (request.body) {
      // Refuse a body that says it is too large before reading any of it,
      // and count the bytes of one that does not say.
      if (declared > limit) return tooLarge(limit);
      const read = await readCapped(request.body, limit);
      if (!read) return tooLarge(limit);
      raw = read;
    }
    const reply = await mock({
      method: request.method,
      url: new URL(request.url),
      header: (name) => request.headers.get(name) ?? undefined,
      body: async () => raw,
    });
    if (!reply) return null;
    const headers = new Headers();
    for (const [k, v] of reply.headers) headers.append(k, v);
    const nullBody = request.method === "HEAD" || [101, 204, 205, 304].includes(reply.status);
    return new Response(nullBody ? null : ((reply.body as BodyInit | undefined) ?? null), {
      status: reply.status,
      headers,
    });
  };
}
