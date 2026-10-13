import type { IncomingMessage, ServerResponse } from "node:http";
import type { operations } from "../src/api/schema";
import { createState, type MockState, randomUUID, type Scenario } from "./data";
import { type Ctx, handlers, problem, type Result } from "./handlers";
import { compileValidators, loadSpec, matchRoute } from "./openapi";
import { resolveFile } from "./resolve";
import { viewer } from "./viewer";

/**
 * A stateful mock of Hangar's HTTP surface for the UI: the native API from
 * api/openapi.yaml (every request and response is validated against it),
 * the Dataset Viewer API, `resolve` downloads and the `/auth/` handler.
 */

type Next = (err?: unknown) => void;

const publicOps = new Set<keyof operations>(["login", "listAuthProviders"]);

/** The Content Security Policy internal/web sets on UI pages. */
export const csp = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
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

function cookies(req: IncomingMessage): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of (req.headers.cookie ?? "").split(/;\s*/)) {
    const i = part.indexOf("=");
    if (i > 0) out[part.slice(0, i)] = decodeURIComponent(part.slice(i + 1));
  }
  return out;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

function send(res: ServerResponse, r: Result, contentType?: string): void {
  for (const [k, v] of Object.entries(r.headers ?? {})) {
    if (k.toLowerCase() === "set-cookie") res.appendHeader(k, v);
    else res.setHeader(k, v);
  }
  res.statusCode = r.status;
  if (r.body === undefined) {
    res.end();
    return;
  }
  const problemType =
    r.status >= 400 && typeof r.body === "object" && r.body !== null && "status" in r.body;
  res.setHeader(
    "Content-Type",
    contentType ?? (problemType ? "application/problem+json" : "application/json"),
  );
  res.end(typeof r.body === "string" ? r.body : JSON.stringify(r.body));
}

export type MockOptions = {
  /** New sessions start signed in, for `pnpm dev:mock`. */
  signedIn?: boolean;
  /** Milliseconds to wait before answering, to show loading states. */
  delay?: number;
};

export function createMock(opts: MockOptions = {}) {
  const { doc, routes } = loadSpec();
  const validators = compileValidators(doc);
  const sessions = new Map<string, MockState>();

  const stateFor = (req: IncomingMessage, res: ServerResponse): MockState => {
    const c = cookies(req);
    let id = c.mock_session;
    if (!id || !sessions.has(id)) {
      id = id || randomUUID();
      const scenario = (
        ["empty", "bulk"].includes(c.mock_scenario ?? "") ? c.mock_scenario : "default"
      ) as Scenario;
      const state = createState(scenario);
      state.signedIn = Boolean(opts.signedIn);
      sessions.set(id, state);
      res.appendHeader("Set-Cookie", `mock_session=${id}; Path=/; SameSite=Lax`);
    }
    return sessions.get(id) as MockState;
  };

  return async function mockMiddleware(
    req: IncomingMessage,
    res: ServerResponse,
    next: Next,
  ): Promise<void> {
    const url = new URL(req.url ?? "/", "http://mock.local");
    const path = url.pathname;
    const method = (req.method ?? "GET").toUpperCase();
    try {
      if (opts.delay) await new Promise((r) => setTimeout(r, opts.delay));
      if (path.startsWith("/api/hangar/v1/exports/")) {
        res.setHeader("Content-Type", "application/zip");
        res.setHeader(
          "Content-Disposition",
          `attachment; filename="${decodeURIComponent(path.split("/").pop() ?? "export.zip")}"`,
        );
        res.end(Buffer.from([0x50, 0x4b, 0x05, 0x06, ...new Array(18).fill(0)]));
        return;
      }
      if (path.startsWith("/api/hangar/v1/")) {
        const state = stateFor(req, res);
        await api(state, req, res, url, method);
        return;
      }
      if (path.startsWith("/datasets-server/")) {
        const state = stateFor(req, res);
        if (!state.signedIn) {
          send(res, { status: 401, body: { error: "Sign in to see this dataset." } });
          return;
        }
        const v = viewer(state, path, url.searchParams);
        if (v?.raw !== undefined) {
          res.setHeader("Content-Type", v.type ?? "application/octet-stream");
          res.end(v.raw);
          return;
        }
        if (v) {
          send(res, v, "application/json");
          return;
        }
      }
      if (path.startsWith("/auth/") && (method === "POST" || path.startsWith("/auth/oidc/"))) {
        const state = stateFor(req, res);
        const handled = await authHandler(state, req, res, url, method);
        if (handled) return;
      }
      if (path.includes("/resolve/")) {
        const state = stateFor(req, res);
        const f = resolveFile(state, path);
        if (f) {
          res.statusCode = f.status;
          for (const [k, v] of Object.entries(f.headers)) res.setHeader(k, v);
          res.end(method === "HEAD" ? undefined : f.body);
          return;
        }
      }
      next();
    } catch (e) {
      console.error("mock:", e);
      send(res, problem(500, `The mock server failed: ${(e as Error).message}`));
    }
  };

  async function api(
    state: MockState,
    req: IncomingMessage,
    res: ServerResponse,
    url: URL,
    method: string,
  ) {
    const rawPath = url.pathname.slice("/api/hangar/v1".length);
    const m = matchRoute(routes, method, rawPath);
    if (!m) {
      send(res, problem(404, `No operation in api/openapi.yaml matches ${method} ${rawPath}.`));
      return;
    }
    const opId = m.route.operationId as keyof operations;
    if (!state.signedIn && !publicOps.has(opId)) {
      send(res, problem(401, "Sign in to continue."));
      return;
    }
    if (
      !["GET", "HEAD", "OPTIONS"].includes(method) &&
      state.signedIn &&
      opId !== "login" &&
      req.headers["x-csrf-token"] !== state.me.csrf_token
    ) {
      send(
        res,
        problem(403, "The X-CSRF-Token header is missing or wrong. Reload the page and try again."),
      );
      return;
    }
    const raw = await readBody(req);
    let body: unknown;
    const type = req.headers["content-type"] ?? "";
    if (raw.length && type.includes("json")) {
      try {
        body = JSON.parse(raw.toString("utf8"));
      } catch {
        send(res, problem(400, "The request body is not valid JSON."));
        return;
      }
    }
    if (m.route.operation.requestBody?.content?.["application/json"]) {
      const errs = validators.request(m.route, body ?? {});
      if (errs) {
        send(
          res,
          problem(
            400,
            "The request does not match the API.",
            errs.map((d) => ({ detail: d })),
          ),
        );
        return;
      }
    }
    const handler = handlers[opId];
    if (!handler) {
      send(res, problem(501, `The mock does not implement ${opId}.`));
      return;
    }
    const ctx: Ctx = { state, params: m.params, query: url.searchParams, body, raw };
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
      res.setHeader("Content-Type", "text/csv; charset=utf-8");
      res.setHeader("Content-Disposition", 'attachment; filename="hangar-audit.csv"');
      res.end(
        [
          "time,actor,action,target_type,target_id,ip",
          ...items.map((a) =>
            [a.created_at, a.actor, a.action, a.target_type, a.target_id, a.ip]
              .map((v) => v ?? "")
              .join(","),
          ),
        ].join("\n"),
      );
      return;
    }
    const errs = validators.response(m.route, result.status, result.body);
    if (errs) {
      console.error(`mock: ${opId} ${result.status} does not match api/openapi.yaml:`, errs);
      send(
        res,
        problem(
          500,
          `The mock's ${opId} response does not match api/openapi.yaml: ${errs.join("; ")}`,
        ),
      );
      return;
    }
    send(res, result);
  }
}

async function authHandler(
  state: MockState,
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  method: string,
): Promise<boolean> {
  const path = url.pathname;
  const oidc = /^\/auth\/oidc\/([^/]+)$/.exec(path);
  if (oidc && method === "GET") {
    state.signedIn = true;
    res.statusCode = 302;
    res.setHeader("Location", url.searchParams.get("redirect") || "/");
    res.end();
    return true;
  }
  const m = /^\/auth\/(register|invite\/([^/]+)|reset\/([^/]+))$/.exec(path);
  if (!m || method !== "POST") return false;
  const raw = await readBody(req);
  const body = raw.length ? (JSON.parse(raw.toString("utf8")) as Record<string, string>) : {};
  const code = m[2] ?? m[3];
  if (code === "expired") {
    send(
      res,
      problem(404, "This link has expired or was already used. Ask an admin for a new one."),
    );
    return true;
  }
  if ((body.password ?? "").length < 10) {
    send(
      res,
      problem(400, "Passwords need at least 10 characters.", [
        { pointer: "/password", detail: "Use at least 10 characters." },
      ]),
    );
    return true;
  }
  if (body.password === "password1234") {
    send(
      res,
      problem(400, "This password is too common. Choose another.", [
        { pointer: "/password", detail: "This password is too common." },
      ]),
    );
    return true;
  }
  state.signedIn = true;
  if (body.username)
    state.me = {
      ...state.me,
      username: body.username,
      display_name: body.display_name || body.username,
    };
  send(res, { status: m[3] ? 200 : 201, body: state.me });
  return true;
}
