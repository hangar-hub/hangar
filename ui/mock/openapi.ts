import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Ajv2020, type ValidateFunction } from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import { parse } from "yaml";

/**
 * Loads api/openapi.yaml and turns it into a router and validators, so the
 * mock answers exactly the operations, paths and shapes the contract has.
 */

type Schema = Record<string, unknown>;
type Operation = {
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

const specPath = fileURLToPath(new URL("../../api/openapi.yaml", import.meta.url));

export function loadSpec(): { doc: Schema; routes: Route[] } {
  const doc = parse(readFileSync(specPath, "utf8")) as Schema;
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
  return { doc, routes };
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

/** Compiles validators for every request body and JSON response in the spec. */
export function compileValidators(doc: Schema): Validators {
  const ajv = new Ajv2020({ strict: false, allErrors: true, validateFormats: true });
  addFormats.default(ajv);
  ajv.addFormat("password", true);
  ajv.addFormat("binary", true);
  ajv.addSchema({ $id: "openapi.json", components: doc.components }, "openapi.json");
  const cache = new Map<string, ValidateFunction>();

  const fix = (schema: Schema): Schema =>
    JSON.parse(
      JSON.stringify(schema).replaceAll('"#/components/', '"openapi.json#/components/'),
    ) as Schema;

  const compile = (key: string, schema: Schema): ValidateFunction => {
    let v = cache.get(key);
    if (!v) {
      v = ajv.compile(fix(schema));
      cache.set(key, v);
    }
    return v;
  };

  const errors = (v: ValidateFunction): string[] =>
    (v.errors ?? []).map((e) => `${e.instancePath || "/"} ${e.message ?? "is invalid"}`);

  const responseSchema = (route: Route, status: number): Schema | undefined => {
    const resp = route.operation.responses[String(status)] ?? route.operation.responses.default;
    if (!resp) return undefined;
    if (resp.$ref) {
      return { $ref: "#/components/schemas/Problem" };
    }
    const content = resp.content ?? {};
    return content["application/json"]?.schema ?? content["application/problem+json"]?.schema;
  };

  return {
    request(route, body) {
      const schema = route.operation.requestBody?.content?.["application/json"]?.schema;
      if (!schema) return null;
      const v = compile(`req:${route.operationId}`, schema);
      return v(body) ? null : errors(v);
    },
    response(route, status, body) {
      if (status === 204) return body === undefined ? null : ["a 204 response has a body"];
      const schema = responseSchema(route, status);
      if (!schema) return [`status ${status} is not documented for ${route.operationId}`];
      const v = compile(`res:${route.operationId}:${status}`, schema);
      return v(body) ? null : errors(v);
    },
  };
}
