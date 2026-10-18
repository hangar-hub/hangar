import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Ajv2020, type Options } from "ajv/dist/2020.js";
import * as standalone from "ajv/dist/standalone/index.js";
import addFormats from "ajv-formats";
import { parse } from "yaml";
import {
  type Route,
  routesOf,
  type Schema,
  schemasOf,
  type ValidateFunction,
  type Validators,
  validatorsFrom,
  withComponentRefs,
} from "./contract.ts";

/**
 * Loads api/openapi.yaml and turns it into a router and validators, so the
 * mock answers exactly the operations, paths and shapes the contract has.
 * The router and the validator logic are in contract.ts, which has no Node
 * APIs; this file reads the spec from disk and compiles the schemas.
 */

export { type Match, matchRoute, pathExists, type Route } from "./contract.ts";

const specPath = fileURLToPath(new URL("../../api/openapi.yaml", import.meta.url));

export function loadSpec(): { doc: Schema; routes: Route[] } {
  const doc = parse(readFileSync(specPath, "utf8")) as Schema;
  return { doc, routes: routesOf(doc) };
}

function newAjv(doc: Schema, opts: Options = {}): Ajv2020 {
  const ajv = new Ajv2020({ strict: false, allErrors: true, validateFormats: true, ...opts });
  addFormats.default(ajv);
  ajv.addFormat("password", true);
  ajv.addFormat("binary", true);
  ajv.addSchema({ $id: "openapi.json", components: doc.components }, "openapi.json");
  return ajv;
}

/** Compiles validators for every request body and JSON response in the spec, on first use. */
export function compileValidators(doc: Schema): Validators {
  const ajv = newAjv(doc);
  const schemas = schemasOf(routesOf(doc));
  const cache = new Map<string, ValidateFunction>();
  return validatorsFrom((key) => {
    let v = cache.get(key);
    if (!v) {
      const schema = schemas.get(key);
      if (!schema) return undefined;
      v = ajv.compile(withComponentRefs(schema)) as ValidateFunction;
      cache.set(key, v);
    }
    return v;
  });
}

/**
 * Returns an ES module with every validator compiled ahead of time, for
 * runtimes that cannot compile code at run time, such as Cloudflare Workers.
 * It exports `index`, which maps each schemasOf key to the name of the
 * module's export that checks it.
 */
export function standaloneValidators(doc: Schema): string {
  const ajv = newAjv(doc, { code: { source: true, esm: true } });
  const index: Record<string, string> = {};
  const exports: Record<string, string> = {};
  let n = 0;
  for (const [key, schema] of schemasOf(routesOf(doc))) {
    const name = `v${n++}`;
    ajv.addSchema({ ...withComponentRefs(schema), $id: `${name}.json` });
    index[key] = name;
    exports[name] = `${name}.json`;
  }
  return `${standalone.default(ajv, exports)}\nexport const index = ${JSON.stringify(index)};\n`;
}
