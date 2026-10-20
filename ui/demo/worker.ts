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

/**
 * Headers every demo response gets, as internal/web and internal/httpx set
 * them, and HSTS, which the demo can always send because Cloudflare serves it
 * over HTTPS only.
 */
const common: Record<string, string> = {
  "X-Content-Type-Options": "nosniff",
  "Referrer-Policy": "strict-origin-when-cross-origin",
  "Strict-Transport-Security": "max-age=31536000",
};

/**
 * The one demo page search engines may index: the home page on the production
 * host, so a search for Hangar can lead to the demo. Every other page is
 * sample data, which is no use in search results, and Previews repeat
 * production, so they get `X-Robots-Tag: noindex`.
 */
export const indexedPage = "https://demo.hangar.page/";

/**
 * The title, description, canonical URL and site name the indexed page gets
 * in place of the UI's title. Google names each host in its results with the
 * site name from WebSite structured data, and the demo is its own host. The
 * WebSite names hangar.page's as its parent and the same publisher.
 */
export const indexedHead =
  "<title>Hangar live demo: the self-hosted model registry</title>" +
  '<meta name="description" content="Try Hangar in your browser with sample data: browse models and datasets, read model cards and preview dataset rows in the self-hosted model registry." />' +
  `<link rel="canonical" href="${indexedPage}" />` +
  '<meta property="og:site_name" content="Hangar demo" />' +
  `<script type="application/ld+json">${JSON.stringify({
    "@context": "https://schema.org",
    "@type": "WebSite",
    name: "Hangar demo",
    alternateName: "Hangar live demo",
    url: indexedPage,
    isPartOf: {
      "@type": "WebSite",
      "@id": "https://hangar.page/#website",
      name: "Hangar",
      url: "https://hangar.page/",
    },
    publisher: {
      "@type": "Organization",
      "@id": "https://hangar.page/#organization",
      name: "Hangar",
      url: "https://hangar.page/",
    },
  })}</script>`;

const isIndexed = (url: URL) => url.origin + url.pathname === indexedPage;

function secured(res: Response, indexable = false): Response {
  const out = new Response(res.body, res);
  for (const [k, v] of Object.entries(common)) {
    if (!out.headers.has(k)) out.headers.set(k, v);
  }
  if (!indexable && !out.headers.has("X-Robots-Tag")) out.headers.set("X-Robots-Tag", "noindex");
  return out;
}

/**
 * The raster favicons every demo page links to, ahead of the UI's SVG, for
 * search results and crawlers that do not read SVG. The Worker serves them
 * from its root files.
 */
export const faviconLinks =
  '<link rel="icon" href="/favicon.ico" sizes="48x48" />' +
  '<link rel="icon" href="/favicon-96.png" sizes="96x96" type="image/png" />';

async function page(env: Env, url: URL, favicons: boolean): Promise<Response> {
  const index = await env.ASSETS.fetch(new Request(new URL("/", url)));
  if (!index.ok) return index;
  const nonce = newNonce();
  let html = withNonce(await index.text(), nonce);
  if (isIndexed(url)) html = html.replace(/<title>[^<]*<\/title>/, indexedHead);
  if (favicons) html = html.replace("</head>", `${faviconLinks}</head>`);
  return new Response(html, { headers: pageHeaders(nonce) });
}

/** The validators in a module that standaloneValidators wrote. */
export function compiledValidators(compiled: CompiledValidators): Validators {
  return validatorsFrom((key) => {
    const name = compiled.index[key];
    return name ? (compiled[name] as ValidateFunction) : undefined;
  });
}

/**
 * Files the demo serves at its root, keyed by path, with their content type
 * and base64 body: the files Google Search Console and Bing Webmaster Tools
 * check ownership with, and the raster favicons. demo/generate.ts reads them
 * from deploy/cloudflare/docs-root, where hangar.page serves the same files.
 * They are not static assets, because the Worker answers every request for
 * HTML with the UI, and search engines may index them.
 */
export type RootFiles = Record<string, { type: string; body: string }>;

function rootFile(file: RootFiles[string], method: string): Response {
  const body = method === "HEAD" ? null : Uint8Array.from(atob(file.body), (c) => c.charCodeAt(0));
  return new Response(body, { headers: { "Content-Type": file.type } });
}

export function createDemoWorker(doc: Schema, validators: Validators, rootFiles: RootFiles = {}) {
  const routes = routesOf(doc);
  const favicons = Object.hasOwn(rootFiles, "/favicon-96.png");
  let mock: ReturnType<typeof createFetchMock> | undefined;

  return {
    async fetch(request: Request, env: Env): Promise<Response> {
      const url = new URL(request.url);
      const file = Object.hasOwn(rootFiles, url.pathname) ? rootFiles[url.pathname] : undefined;
      if (file !== undefined && (request.method === "GET" || request.method === "HEAD")) {
        return secured(rootFile(file, request.method), true);
      }
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
      const answer = await mock(request);
      if (answer) return secured(answer);
      // Crawlers such as Googlebot send Accept: */*, so the indexed page does
      // not wait for text/html, or they would get the bare index.html.
      if (isIndexed(url) && (request.method === "GET" || request.method === "HEAD")) {
        return secured(await page(env, url, favicons), true);
      }
      if (isUiPage(request.method, url.pathname, request.headers.get("accept") ?? "")) {
        return secured(await page(env, url, favicons));
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
