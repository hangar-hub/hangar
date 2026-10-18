import type { operations } from "../src/api/schema";
import { matchRoute, type Route, type Validators } from "./contract.ts";
import { createState, type MockState, randomUUID } from "./data.ts";
import { type Ctx, handlers, problem, type Result } from "./handlers.ts";
import { resolveFile } from "./resolve.ts";
import { viewer } from "./viewer.ts";

/**
 * A stateful mock of Hangar's HTTP surface for the UI: the native API from
 * api/openapi.yaml (every request and response is validated against it),
 * the Dataset Viewer API, `resolve` downloads and the `/auth/` handler.
 *
 * It knows nothing of the transport: server.ts adapts it to Node's HTTP
 * server for Vite, and fetch.ts to the Fetch API for the demo's Cloudflare
 * Worker. Each visitor gets their own copy of the fixture data, keyed by
 * the `mock_session` cookie.
 */

/** A request, as an adapter hands it to the mock. */
export type MockRequest = {
  method: string;
  url: URL;
  /** A request header by lower-case name. */
  header(name: string): string | undefined;
  /** The request body; adapters read it only when the mock asks. */
  body(): Promise<Buffer>;
};

/** The mock's answer. Set-Cookie may repeat, so headers are a list. */
export type MockReply = {
  status: number;
  headers: [string, string][];
  body?: string | Uint8Array;
};

export type SessionLimits = {
  /** Request body bytes one session may send; past it, writes answer 413. */
  sessionBytes: number;
  /**
   * Bytes all sessions may hold, counting each session as `stateBytes`
   * plus the request bodies it sent. Past it, the least recently used
   * sessions are dropped, and their next request starts from the fixtures.
   */
  totalBytes: number;
  /** A session unused for this long is dropped. */
  idleMs: number;
};

export type MockOptions = {
  /** New sessions start signed in, for `pnpm dev:mock` and the demo. */
  signedIn?: boolean;
  /** Milliseconds to wait before answering, to show loading states. */
  delay?: number;
  /** Bounds on the sessions the mock keeps in memory. None by default. */
  limits?: SessionLimits;
};

/** What one session's fixture data costs in memory, rounded up (about 71 KiB measured). */
export const stateBytes = 128 * 1024;

const publicOps = new Set<keyof operations>(["login", "listAuthProviders", "getMe"]);

type Session = { state: MockState; bytes: number; seen: number };

/**
 * Keeps only paths on this site, as internal/auth.SafeRedirect does, so the
 * OIDC sign-in cannot send a visitor to another site: the target must start
 * with "/" but not "//" or "/\\", have no control characters, be at most
 * 2000 characters, and still have this origin once resolved against it.
 * Anything else becomes "/".
 */
export function safeRedirect(to: string | null | undefined, origin: string): string {
  if (
    !to?.startsWith("/") ||
    to.startsWith("//") ||
    to.startsWith("/\\") ||
    // biome-ignore lint/suspicious/noControlCharactersInRegex: control characters are what it refuses.
    /[\u0000-\u001f\u007f]/.test(to) ||
    to.length > 2000
  ) {
    return "/";
  }
  try {
    return new URL(to, origin).origin === origin ? to : "/";
  } catch {
    return "/";
  }
}

/** Drops the characters that would end or break a quoted header value. */
function headerSafe(s: string): string {
  // biome-ignore lint/suspicious/noControlCharactersInRegex: control characters are what it drops.
  return s.replace(/["\\\u0000-\u001f\u007f]/g, "");
}

function cookies(header: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of (header ?? "").split(/;\s*/)) {
    const i = part.indexOf("=");
    if (i > 0) out[part.slice(0, i)] = decodeURIComponent(part.slice(i + 1));
  }
  return out;
}

/** Collects one reply's status, headers and body. */
class Reply {
  status = 200;
  headers: [string, string][] = [];
  body?: string | Uint8Array;

  set(name: string, value: string): void {
    const lower = name.toLowerCase();
    this.headers = this.headers.filter(([k]) => k.toLowerCase() !== lower);
    this.headers.push([name, value]);
  }

  append(name: string, value: string): void {
    this.headers.push([name, value]);
  }

  /** Sends a handler result as JSON, or as a problem for an error status. */
  send(r: Result, contentType?: string): MockReply {
    for (const [k, v] of Object.entries(r.headers ?? {})) {
      if (k.toLowerCase() === "set-cookie") this.append(k, v);
      else this.set(k, v);
    }
    this.status = r.status;
    if (r.body !== undefined) {
      const problemType =
        r.status >= 400 && typeof r.body === "object" && r.body !== null && "status" in r.body;
      this.set(
        "Content-Type",
        contentType ?? (problemType ? "application/problem+json" : "application/json"),
      );
      this.body = typeof r.body === "string" ? r.body : JSON.stringify(r.body);
    }
    return this.done();
  }

  done(): MockReply {
    return { status: this.status, headers: this.headers, body: this.body };
  }
}

export type MockHandler = (req: MockRequest) => Promise<MockReply | null>;

/**
 * Returns the mock: it answers a request it handles, and returns null for
 * one it does not, such as a UI page or an asset.
 */
export function createMockCore(
  contract: { routes: Route[]; validators: Validators },
  opts: MockOptions = {},
): MockHandler {
  const { routes, validators } = contract;
  const sessions = new Map<string, Session>();
  let held = 0;

  const drop = (id: string) => {
    const s = sessions.get(id);
    if (!s) return;
    held -= stateBytes + s.bytes;
    sessions.delete(id);
  };

  /** Drops idle sessions, then the least recently used ones over the total. */
  const prune = (now: number, keep: string) => {
    const { limits } = opts;
    if (!limits) return;
    // The Map keeps sessions in order of last use, oldest first.
    for (const [id, s] of sessions) {
      if (id === keep) continue;
      if (now - s.seen < limits.idleMs && held <= limits.totalBytes) break;
      drop(id);
    }
  };

  const sessionFor = (req: MockRequest, reply: Reply): Session => {
    let id = cookies(req.header("cookie")).mock_session;
    let s = id ? sessions.get(id) : undefined;
    if (!id || !s) {
      id = id || randomUUID();
      const state = createState();
      state.signedIn = Boolean(opts.signedIn);
      s = { state, bytes: 0, seen: 0 };
      held += stateBytes;
      const secure = req.url.protocol === "https:" ? "; Secure" : "";
      reply.append("Set-Cookie", `mock_session=${id}; Path=/; SameSite=Lax${secure}`);
    }
    // Move the session to the end of the Map: the most recently used.
    sessions.delete(id);
    s.seen = Date.now();
    sessions.set(id, s);
    prune(s.seen, id);
    return s;
  };

  /** Reads the body into the session's byte count, or answers 413 past its limit. */
  const readBody = async (s: Session, req: MockRequest): Promise<Buffer | MockReply> => {
    const raw = await req.body();
    const limit = opts.limits?.sessionBytes;
    if (limit !== undefined && raw.length > 0) {
      if (s.bytes + raw.length > limit) {
        return new Reply().send(
          problem(
            413,
            "This session has sent all the data the demo keeps. Clear this site's cookies to start again with the sample data.",
          ),
        );
      }
      s.bytes += raw.length;
      held += raw.length;
    }
    return raw;
  };

  return async function mock(req: MockRequest): Promise<MockReply | null> {
    const { url } = req;
    const path = url.pathname;
    const method = req.method.toUpperCase();
    const reply = new Reply();
    try {
      if (opts.delay) await new Promise((r) => setTimeout(r, opts.delay));
      if (path.startsWith("/api/hangar/v1/exports/")) {
        reply.set("Content-Type", "application/zip");
        reply.set(
          "Content-Disposition",
          `attachment; filename="${headerSafe(decodeURIComponent(path.split("/").pop() ?? "")) || "export.zip"}"`,
        );
        reply.body = new Uint8Array([0x50, 0x4b, 0x05, 0x06, ...new Array(18).fill(0)]);
        return reply.done();
      }
      if (path.startsWith("/api/hangar/v1/")) {
        return await api(sessionFor(req, reply), req, reply, url, method);
      }
      if (path.startsWith("/datasets-server/")) {
        const { state } = sessionFor(req, reply);
        if (!state.signedIn) {
          return reply.send({ status: 401, body: { error: "Sign in to see this dataset." } });
        }
        const v = viewer(state, path, url.searchParams);
        if (v?.raw !== undefined) {
          reply.set("Content-Type", v.type ?? "application/octet-stream");
          reply.body = v.raw;
          return reply.done();
        }
        if (v) return reply.send(v, "application/json");
      }
      if (path.startsWith("/auth/") && (method === "POST" || path.startsWith("/auth/oidc/"))) {
        const handled = await authHandler(sessionFor(req, reply), req, reply, url, method);
        if (handled) return handled;
      }
      if (path.includes("/resolve/")) {
        const { state } = sessionFor(req, reply);
        const f = resolveFile(state, path);
        if (f) {
          reply.status = f.status;
          for (const [k, v] of Object.entries(f.headers)) reply.set(k, v);
          if (method !== "HEAD") reply.body = f.body;
          return reply.done();
        }
      }
      return null;
    } catch (e) {
      console.error("mock:", e);
      return new Reply().send(problem(500, `The mock server failed: ${(e as Error).message}`));
    }
  };

  async function api(
    s: Session,
    req: MockRequest,
    reply: Reply,
    url: URL,
    method: string,
  ): Promise<MockReply> {
    const { state } = s;
    const rawPath = url.pathname.slice("/api/hangar/v1".length);
    const m = matchRoute(routes, method, rawPath);
    if (!m) {
      return reply.send(
        problem(404, `No operation in api/openapi.yaml matches ${method} ${rawPath}.`),
      );
    }
    const opId = m.route.operationId as keyof operations;
    if (!state.signedIn && !publicOps.has(opId)) {
      return reply.send(problem(401, "Sign in to continue."));
    }
    if (
      !["GET", "HEAD", "OPTIONS"].includes(method) &&
      state.signedIn &&
      opId !== "login" &&
      req.header("x-csrf-token") !== state.me.csrf_token
    ) {
      return reply.send(
        problem(403, "The X-CSRF-Token header is missing or wrong. Reload the page and try again."),
      );
    }
    const raw = await readBody(s, req);
    if (!Buffer.isBuffer(raw)) return raw;
    let body: unknown;
    const type = req.header("content-type") ?? "";
    if (raw.length && type.includes("json")) {
      try {
        body = JSON.parse(raw.toString("utf8"));
      } catch {
        return reply.send(problem(400, "The request body is not valid JSON."));
      }
    }
    if (m.route.operation.requestBody?.content?.["application/json"]) {
      const errs = validators.request(m.route, body ?? {});
      if (errs) {
        return reply.send(
          problem(
            400,
            "The request does not match the API.",
            errs.map((d) => ({ detail: d })),
          ),
        );
      }
    }
    const handler = handlers[opId];
    if (!handler) {
      return reply.send(problem(501, `The mock does not implement ${opId}.`));
    }
    const ctx: Ctx = {
      state,
      origin: url.origin,
      params: m.params,
      query: url.searchParams,
      body,
      raw,
    };
    const result = handler(ctx);
    if (
      opId === "adminListAudit" &&
      url.searchParams.get("format") === "csv" &&
      result.status === 200
    ) {
      const items = (
        result.body as {
          items: {
            created_at: string;
            actor?: string;
            action: string;
            target_type?: string;
            target_id?: string;
            ip?: string;
          }[];
        }
      ).items;
      reply.set("Content-Type", "text/csv; charset=utf-8");
      reply.set("Content-Disposition", 'attachment; filename="hangar-audit.csv"');
      reply.body = [
        "time,actor,action,target_type,target_id,ip",
        ...items.map((a) =>
          [a.created_at, a.actor, a.action, a.target_type, a.target_id, a.ip]
            .map((v) => v ?? "")
            .join(","),
        ),
      ].join("\n");
      return reply.done();
    }
    const errs = validators.response(m.route, result.status, result.body);
    if (errs) {
      console.error(`mock: ${opId} ${result.status} does not match api/openapi.yaml:`, errs);
      return reply.send(
        problem(
          500,
          `The mock's ${opId} response does not match api/openapi.yaml: ${errs.join("; ")}`,
        ),
      );
    }
    return reply.send(result);
  }

  async function authHandler(
    s: Session,
    req: MockRequest,
    reply: Reply,
    url: URL,
    method: string,
  ): Promise<MockReply | null> {
    const { state } = s;
    const path = url.pathname;
    const oidc = /^\/auth\/oidc\/([^/]+)$/.exec(path);
    if (oidc && method === "GET") {
      state.signedIn = true;
      reply.status = 302;
      reply.set("Location", safeRedirect(url.searchParams.get("redirect"), url.origin));
      return reply.done();
    }
    const m = /^\/auth\/(register|invite\/([^/]+)|reset\/([^/]+))$/.exec(path);
    if (!m || method !== "POST") return null;
    const raw = await readBody(s, req);
    if (!Buffer.isBuffer(raw)) return raw;
    const body = raw.length ? (JSON.parse(raw.toString("utf8")) as Record<string, string>) : {};
    const code = m[2] ?? m[3];
    // Like W5's Register and ResetPassword: the link must exist and be unused
    // before the password is checked.
    const kind = m[2] ? "invite" : m[3] ? "reset" : undefined;
    if (code !== undefined && state.links[code] !== kind) {
      return reply.send(
        problem(404, "The link is invalid, used or expired; ask an admin for a new one."),
      );
    }
    if ((body.password ?? "").length < 10) {
      return reply.send(
        problem(400, "Passwords need at least 10 characters.", [
          { pointer: "/password", detail: "Use at least 10 characters." },
        ]),
      );
    }
    if (body.password === "password1234") {
      return reply.send(
        problem(400, "This password is too common. Choose another.", [
          { pointer: "/password", detail: "This password is too common." },
        ]),
      );
    }
    if (code !== undefined) delete state.links[code];
    state.signedIn = true;
    if (body.username)
      state.me = {
        ...state.me,
        username: body.username,
        display_name: body.display_name || body.username,
      };
    return reply.send({ status: m[3] ? 200 : 201, body: state.me });
  }
}
