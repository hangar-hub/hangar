/**
 * The parts of api/openapi.yaml the mock works from, with no Node APIs, so
 * the same code runs under Node (mock/openapi.ts) and in the demo's
 * Cloudflare Worker (demo/worker.ts): a router built from the spec's paths,
 * and validators for every request body and JSON response.
 */

export type Schema = Record<string, unknown>;
export type Operation = {
  operationId: string;
  requestBody?: { content?: Record<string, { schema?: Schema }> };
  responses: Record<string, { $ref?: string; content?: Record<string, { schema?: Schema }> }>;
};

export type Route = {
  method: string;
  template: string;
  operationId: string;
  segments: string[];
  operation: Operation;
};

export type Match = { route: Route; params: Record<string, string> };

/** Lists the spec's operations as routes, static segments first. */
export function routesOf(doc: Schema): Route[] {
  const paths = doc.paths as Record<string, Record<string, Operation>>;
  const routes: Route[] = [];
  for (const [template, item] of Object.entries(paths)) {
    for (const [method, op] of Object.entries(item)) {
      if (method === "parameters" || typeof op !== "object" || !op.operationId) continue;
      routes.push({
        method: method.toUpperCase(),
        template,
        operationId: op.operationId,
        segments: template.split("/").filter(Boolean),
        operation: op,
      });
    }
  }
  // Static segments win over parameters: /repos/datasets/{ns}/{name}/configs
  // before /repos/{type}/{ns}/{name}/....
  const staticCount = (r: Route) => r.segments.filter((s) => !s.startsWith("{")).length;
  routes.sort((a, b) => staticCount(b) - staticCount(a));
  return routes;
}

/** Matches a raw path (still percent-encoded) to a route. */
export function matchRoute(routes: Route[], method: string, rawPath: string): Match | null {
  const parts = rawPath.split("/").filter(Boolean);
  for (const route of routes) {
    if (route.method !== method || route.segments.length !== parts.length) continue;
    const params: Record<string, string> = {};
    let ok = true;
    for (let i = 0; i < parts.length; i++) {
      const seg = route.segments[i] as string;
      const part = parts[i] as string;
      if (seg.startsWith("{")) {
        params[seg.slice(1, -1)] = decodeURIComponent(part);
      } else if (seg !== part) {
        ok = false;
        break;
      }
    }
    if (ok) return { route, params };
  }
  return null;
}

/** True when some route has the path with another method, for 405 answers. */
export function pathExists(routes: Route[], rawPath: string): boolean {
  return routes.some((r) => matchRoute([{ ...r }], r.method, rawPath));
}

export type Validators = {
  request(route: Route, body: unknown): string[] | null;
  response(route: Route, status: number, body: unknown): string[] | null;
};

/** A compiled JSON Schema check, as Ajv returns it at run time or in standalone code. */
export type ValidateFunction = {
  (data: unknown): boolean;
  errors?: { instancePath: string; message?: string }[] | null;
};

const requestKey = (route: Route) => `req:${route.operationId}`;

/** The key of the response the spec documents for status: its own or `default`. */
function responseKey(route: Route, status: number): string | undefined {
  const { responses } = route.operation;
  const code = String(status) in responses ? String(status) : "default";
  return code in responses ? `res:${route.operationId}:${code}` : undefined;
}

/**
 * Every schema the validators check, by key: each JSON request body, and
 * each documented response with a JSON body. A response given as a `$ref`
 * is a shared problem response.
 */
export function schemasOf(routes: Route[]): Map<string, Schema> {
  const out = new Map<string, Schema>();
  for (const route of routes) {
    const req = route.operation.requestBody?.content?.["application/json"]?.schema;
    if (req) out.set(requestKey(route), req);
    for (const [code, resp] of Object.entries(route.operation.responses)) {
      const content = resp.content ?? {};
      const schema = resp.$ref
        ? { $ref: "#/components/schemas/Problem" }
        : (content["application/json"]?.schema ?? content["application/problem+json"]?.schema);
      if (schema) out.set(`res:${route.operationId}:${code}`, schema);
    }
  }
  return out;
}

/** Points `#/components/` references at the components schema, which has the $id openapi.json. */
export function withComponentRefs(schema: Schema): Schema {
  return JSON.parse(
    JSON.stringify(schema).replaceAll('"#/components/', '"openapi.json#/components/'),
  ) as Schema;
}

/** Builds the validators from a lookup of compiled checks by schemasOf key. */
export function validatorsFrom(lookup: (key: string) => ValidateFunction | undefined): Validators {
  const errors = (v: ValidateFunction): string[] =>
    (v.errors ?? []).map((e) => `${e.instancePath || "/"} ${e.message ?? "is invalid"}`);
  return {
    request(route, body) {
      const v = lookup(requestKey(route));
      if (!v) return null;
      return v(body) ? null : errors(v);
    },
    response(route, status, body) {
      if (status === 204) return body === undefined ? null : ["a 204 response has a body"];
      const key = responseKey(route, status);
      const v = key ? lookup(key) : undefined;
      if (!v) return [`status ${status} is not documented for ${route.operationId}`];
      return v(body) ? null : errors(v);
    },
  };
}
