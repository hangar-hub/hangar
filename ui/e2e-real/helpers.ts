import {
  type APIRequestContext,
  type APIResponse,
  expect,
  type Page,
  request,
  test,
} from "@playwright/test";
import { unique } from "../e2e/helpers";
import { gguf } from "./gguf";

// Shared by seed.ts and the e2e-real specs: sessions on the Instance under
// test and on the second Instance that is its Upstream.

export const api = "/api/hangar/v1";

/** The user the seed makes on the Instance under test. */
export const user = { username: "ada", password: "correct-horse-battery" };

/** The Upstream the seed adds: a second Hangar Instance. */
export const upstreamName = "e2e-upstream";

/** The Organisation on the Upstream that holds the GGUF Repos the flows pull. */
export const upstreamNamespace = "lab";

export function env(name: string): string {
  const v = process.env[name];
  if (!v) {
    throw new Error(
      `Set ${name}. The e2e-real suite needs HANGAR_URL and HANGAR_E2E_ADMIN_PASSWORD for the Instance under test, and HANGAR_E2E_UPSTREAM_URL and HANGAR_E2E_UPSTREAM_ADMIN_PASSWORD for a second Instance it uses as its Upstream.`,
    );
  }
  return v;
}

/** A signed-in API session: `call` sends the CSRF token on state-changing calls. */
export type Session = {
  ctx: APIRequestContext;
  call(
    method: "GET" | "POST" | "PATCH" | "DELETE",
    path: string,
    data?: unknown,
  ): Promise<APIResponse>;
  dispose(): Promise<void>;
};

/** The longest sign-in lock (docs/spec/05-auth.md, "Local accounts"). */
const longestLock = 15 * 60_000;

/** Throttled sign-ins in a row before a caller gives up and reports the lock. */
const lockTries = 3;

/**
 * How long a sign-in answer asks the caller to wait, in milliseconds: the
 * Retry-After of a 429, plus half a second for its rounding to whole
 * seconds. Undefined for any other answer. Sign-in throttling counts
 * failures per IP address for an hour (docs/spec/05-auth.md), and the
 * sign-in flow makes one at each width, all from this host, so a rerun
 * within the hour meets locks of a second, then two, four and so on.
 */
export function signInLock(res: {
  status(): number;
  headers(): Record<string, string>;
}): number | undefined {
  if (res.status() !== 429) return undefined;
  const seconds = Number(res.headers()["retry-after"]);
  if (!Number.isFinite(seconds) || seconds < 0) return undefined;
  return Math.min(seconds * 1000, longestLock) + 500;
}

/** Waits out a sign-in lock, adding the wait to the running test's timeout. */
export async function waitOutLock(ms: number): Promise<void> {
  let info: ReturnType<typeof test.info> | undefined;
  try {
    info = test.info();
  } catch {
    // seed.ts runs as global setup, outside any test, with no timeout to extend.
  }
  if (info && info.timeout > 0) info.setTimeout(info.timeout + ms);
  await new Promise((r) => setTimeout(r, ms));
}

/** Signs in through the API, waiting out a lock from earlier failures on this host. */
async function postLogin(
  ctx: APIRequestContext,
  username: string,
  password: string,
): Promise<APIResponse> {
  for (let tries = 1; ; tries++) {
    const res = await ctx.post(`${api}/auth/login`, { data: { username, password } });
    const lock = signInLock(res);
    if (lock === undefined || tries === lockTries) return res;
    await waitOutLock(lock);
  }
}

export async function session(
  baseURL: string,
  username: string,
  password: string,
): Promise<Session> {
  const ctx = await request.newContext({ baseURL });
  const res = await postLogin(ctx, username, password);
  expect(res.ok(), `sign in as ${username} on ${baseURL}: ${await res.text()}`).toBe(true);
  const csrf = (await res.json()).csrf_token as string;
  return {
    ctx,
    call: (method, path, data) =>
      ctx.fetch(`${api}${path}`, {
        method,
        data,
        headers: method === "GET" ? {} : { "X-CSRF-Token": csrf },
      }),
    dispose: () => ctx.dispose(),
  };
}

/** Starts a new browser session signed in as the user. */
export async function signIn(page: Page, username = user.username, password = user.password) {
  // Sign-in with a session cookie needs its CSRF token, so start afresh.
  await page.context().clearCookies();
  const res = await postLogin(page.request, username, password);
  expect(res.ok(), await res.text()).toBe(true);
}

/** Polls `check` until it is true, for work the Instance does in Jobs. */
export async function waitFor(what: string, check: () => Promise<boolean>, timeout = 120_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`timed out waiting for ${what}`);
}

/** Fails unless the response is a success, or a 409 for something that exists. */
export async function ensure(res: APIResponse, what: string): Promise<void> {
  if (res.ok() || res.status() === 409) return;
  throw new Error(`${what} answered ${res.status()}: ${await res.text()}`);
}

export const quants = ["Q4_K_M", "Q8_0"] as const;

/**
 * Makes a new public GGUF Repo with a Q4_K_M and a Q8_0 file on the
 * Upstream, so each run of a Pull flow starts with nothing of it in the
 * arsenal. Returns its ID and the file name of each quant.
 */
export async function upstreamGgufRepo(): Promise<{
  id: string;
  name: string;
  file: Record<(typeof quants)[number], string>;
}> {
  const up = await session(
    env("HANGAR_E2E_UPSTREAM_URL"),
    "hangar-admin",
    env("HANGAR_E2E_UPSTREAM_ADMIN_PASSWORD"),
  );
  try {
    const name = `tiny-gguf-${unique()}`;
    const id = `${upstreamNamespace}/${name}`;
    const created = await up.call("POST", "/repos", {
      type: "model",
      namespace: upstreamNamespace,
      name,
      visibility: "public",
    });
    expect(created.status(), await created.text()).toBe(201);
    const add = (path: string, content: Buffer | string) => ({
      op: "add",
      path,
      content_base64: Buffer.from(content).toString("base64"),
    });
    // Hangar stores a file in LFS by the parent Commit's .gitattributes, so
    // track *.gguf first, as on huggingface.co. Only LFS files can be left
    // out of a Partial mirror.
    const track = await up.call("POST", `/repos/models/${id}/commits`, {
      summary: "Track GGUF files with LFS",
      operations: [
        add(".gitattributes", "*.gguf filter=lfs diff=lfs merge=lfs -text\n"),
        add("README.md", `---\nlicense: mit\n---\n# ${name}\n\nA tiny llama in two quants.\n`),
      ],
    });
    expect(track.status(), await track.text()).toBe(201);
    const file = { Q4_K_M: `${name}.Q4_K_M.gguf`, Q8_0: `${name}.Q8_0.gguf` };
    const weights = await up.call("POST", `/repos/models/${id}/commits`, {
      summary: "Add the Q4_K_M and Q8_0 quants",
      operations: quants.map((q) => add(file[q], gguf(name, q))),
    });
    expect(weights.status(), await weights.text()).toBe(201);
    return { id, name, file };
  } finally {
    await up.dispose();
  }
}

/** Waits until the arsenal's Mirror of `id` holds `path`. */
export async function waitForPulledFile(s: Session, id: string, path: string): Promise<void> {
  await waitFor(`${path} in the arsenal`, async () => {
    const res = await s.call("GET", `/repos/models/${id}/tree/main`);
    if (!res.ok()) return false;
    const items = (await res.json()).items as { path: string; present?: boolean }[];
    return items.some((e) => e.path === path && e.present === true);
  });
}
