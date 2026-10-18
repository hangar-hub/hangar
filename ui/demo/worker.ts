import {
  routesOf,
  type Schema,
  type ValidateFunction,
  type Validators,
  validatorsFrom,
} from "../mock/contract.ts";
import { createMockCore, type SessionLimits } from "../mock/core.ts";
import { createFetchMock } from "../mock/fetch.ts";
import { problem } from "../mock/handlers.ts";
import { isUiPage, newNonce, pageHeaders, withNonce } from "../mock/page.ts";

/**
 * The Cloudflare Worker behind https://demo.hangar.page/: the web UI, built
 * with VITE_HANGAR_DEMO=1, in front of the mock API the Playwright suite
 * uses (mock/core.ts). Static assets are served by Workers static assets;
 * everything else comes here (wrangler.jsonc, run_worker_first).
 *
 * Each visitor's data lives in this isolate's memory and nowhere else, keyed
 * by the mock_session cookie, so it resets when the isolate is recycled, after
 * an hour unused, or when the isolate needs the memory for other visitors.
 * File bytes are never kept: upload parts are counted and dropped, and
 * request bodies are capped (maxBodyBytes, limits).
 */

export type Env = {
  /** The Workers static assets binding: the built UI in demo/dist. */
  ASSETS: { fetch(request: Request): Promise<Response> };
  /** "0" starts new sessions signed out, as the Playwright suite expects. */
  DEMO_SIGNED_IN?: string;
  /**
   * Raises the body cap and the per-session limit to this many MiB, and the
   * total to 16 times that, so parallel tests do not push each other out. Only
   * the Playwright suite's Worker sets it, because the suite uploads a 6 MiB
   * file in one part (playwright.demo.config.ts); demo.hangar.page keeps
   * maxBodyBytes and limits.
   */
  DEMO_TEST_LIMIT_MIB?: string;
};

/** A module from mock/openapi.ts standaloneValidators: compiled checks and their index. */
export type CompiledValidators = { index: Record<string, string> } & Record<string, unknown>;

const MiB = 1024 * 1024;

/** The largest request body the demo reads. */
export const maxBodyBytes = 1 * MiB;

/** Bounds on the data the demo keeps for its visitors, in one isolate's 128 MB. */
export const limits: SessionLimits = {
  sessionBytes: 8 * MiB,
  totalBytes: 64 * MiB,
  idleMs: 60 * 60 * 1000,
};

/** Headers every demo response gets, as internal/web and internal/httpx set them. */
const common: Record<string, string> = {
  "X-Content-Type-Options": "nosniff",
  "Referrer-Policy": "strict-origin-when-cross-origin",
  // Sample data is no use in search results.
  "X-Robots-Tag": "noindex",
};

function secured(res: Response): Response {
  const out = new Response(res.body, res);
  for (const [k, v] of Object.entries(common)) {
    if (!out.headers.has(k)) out.headers.set(k, v);
  }
  return out;
}

async function page(env: Env, url: URL): Promise<Response> {
  const index = await env.ASSETS.fetch(new Request(new URL("/", url)));
  if (!index.ok) return index;
  const nonce = newNonce();
  return new Response(withNonce(await index.text(), nonce), { headers: pageHeaders(nonce) });
}

/** The validators in a module that standaloneValidators wrote. */
export function compiledValidators(compiled: CompiledValidators): Validators {
  return validatorsFrom((key) => {
    const name = compiled.index[key];
    return name ? (compiled[name] as ValidateFunction) : undefined;
  });
}

export function createDemoWorker(doc: Schema, validators: Validators) {
  const routes = routesOf(doc);
  let mock: ReturnType<typeof createFetchMock> | undefined;

  return {
    async fetch(request: Request, env: Env): Promise<Response> {
      if (!mock) {
        const test = Number(env.DEMO_TEST_LIMIT_MIB ?? 0) * MiB;
        mock = createFetchMock(
          createMockCore(
            { routes, validators },
            {
              signedIn: env.DEMO_SIGNED_IN !== "0",
              limits: test ? { ...limits, sessionBytes: test, totalBytes: 16 * test } : limits,
            },
          ),
          { maxBodyBytes: test || maxBodyBytes },
        );
      }
      const url = new URL(request.url);
      const answer = await mock(request);
      if (answer) return secured(answer);
      if (isUiPage(request.method, url.pathname, request.headers.get("accept") ?? "")) {
        return secured(await page(env, url));
      }
      if (request.method !== "GET" && request.method !== "HEAD") {
        const p = problem(405, "Method not allowed.");
        return secured(
          new Response(JSON.stringify(p.body), {
            status: 405,
            headers: { "Content-Type": "application/problem+json", Allow: "GET, HEAD" },
          }),
        );
      }
      return secured(await env.ASSETS.fetch(request));
    },
  };
}
