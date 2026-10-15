import { type APIRequestContext, expect, request } from "@playwright/test";

// Seeds a real Hangar for the e2e-real suite through its native API: the
// user ada, a model with a safetensors file and a pickle file the scan
// marks unsafe, and a dataset with a CSV file the Viewer converts. It waits
// for the scan and the conversion, so the flows see finished results.
// Reruns against the same Instance reuse what is there.

export const user = { username: "ada", password: "correct-horse-battery" };
export const modelRepo = "ada/tiny-model";
export const datasetRepo = "ada/tiny-data";

const api = "/api/hangar/v1";

/** The safetensors header and data of one F32 tensor of two values. */
function safetensors(): Buffer {
  const header = JSON.stringify({
    __metadata__: { format: "pt" },
    "linear.weight": { dtype: "F32", shape: [2], data_offsets: [0, 8] },
  });
  const len = Buffer.alloc(8);
  len.writeBigUInt64LE(BigInt(Buffer.byteLength(header)));
  const data = Buffer.alloc(8);
  data.writeFloatLE(1.5, 0);
  data.writeFloatLE(-2, 4);
  return Buffer.concat([len, Buffer.from(header), data]);
}

/** A protocol 0 pickle that imports os.system: the scan marks it unsafe. */
const evilPickle = Buffer.from("cos\nsystem\n(S'echo hangar'\ntR.", "latin1");

function csv(): Buffer {
  const rows = ["id,animal,legs"];
  const animals = ["cat", "spider", "bird", "fish", "snake"];
  const legs = [4, 8, 2, 0, 0];
  for (let i = 0; i < 40; i++) rows.push(`${i},${animals[i % 5]},${legs[i % 5]}`);
  return Buffer.from(`${rows.join("\n")}\n`);
}

/** Signs in and returns the CSRF token for state-changing calls. */
async function signIn(ctx: APIRequestContext, username: string, password: string): Promise<string> {
  const res = await ctx.post(`${api}/auth/login`, { data: { username, password } });
  expect(res.ok(), `sign in as ${username}: ${await res.text()}`).toBe(true);
  return (await res.json()).csrf_token as string;
}

async function ensure(res: Awaited<ReturnType<APIRequestContext["post"]>>, what: string) {
  if (res.ok() || res.status() === 409) return;
  throw new Error(`${what} answered ${res.status()}: ${await res.text()}`);
}

async function commitOnce(
  ctx: APIRequestContext,
  csrf: string,
  type: "model" | "dataset",
  id: string,
  files: Record<string, Buffer>,
) {
  const [namespace, name] = id.split("/");
  const created = await ctx.post(`${api}/repos`, {
    headers: { "X-CSRF-Token": csrf },
    data: { type, namespace, name, visibility: "public" },
  });
  await ensure(created, `create ${id}`);
  if (created.status() === 409) return;
  const operations = Object.entries(files).map(([path, b]) => ({
    op: "add",
    path,
    content_base64: b.toString("base64"),
  }));
  const res = await ctx.post(`${api}/repos/${type}s/${id}/commits`, {
    headers: { "X-CSRF-Token": csrf },
    data: { summary: "Add files", operations },
  });
  await ensure(res, `commit to ${id}`);
}

async function waitFor(what: string, check: () => Promise<boolean>) {
  const deadline = Date.now() + 120_000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`timed out waiting for ${what}`);
}

export default async function seed() {
  const baseURL = process.env.HANGAR_URL;
  const adminPassword = process.env.HANGAR_E2E_ADMIN_PASSWORD;
  if (!baseURL || !adminPassword) {
    throw new Error("Set HANGAR_URL and HANGAR_E2E_ADMIN_PASSWORD (the bootstrap admin password).");
  }
  const admin = await request.newContext({ baseURL });
  const adminCsrf = await signIn(admin, "hangar-admin", adminPassword);
  await ensure(
    await admin.post(`${api}/admin/users`, {
      headers: { "X-CSRF-Token": adminCsrf },
      data: { ...user, email: "ada@example.com", display_name: "Ada Lovelace" },
    }),
    "create ada",
  );
  // Reruns start with unsafe downloads allowed, the default.
  await ensure(
    await admin.patch(`${api}/admin/settings`, {
      headers: { "X-CSRF-Token": adminCsrf },
      data: { scan_block_unsafe_downloads: false },
    }),
    "reset settings",
  );
  await admin.dispose();

  const ada = await request.newContext({ baseURL });
  const csrf = await signIn(ada, user.username, user.password);
  await commitOnce(ada, csrf, "model", modelRepo, {
    "README.md": Buffer.from(
      "---\nlicense: mit\n---\n# Tiny model\n\nA model for end-to-end tests.\n",
    ),
    "config.json": Buffer.from('{"model_type": "bert"}\n'),
    "model.safetensors": safetensors(),
    "evil.pkl": evilPickle,
  });
  await commitOnce(ada, csrf, "dataset", datasetRepo, {
    "README.md": Buffer.from("---\nlicense: cc-by-4.0\n---\n# Tiny data\n"),
    "data/train.csv": csv(),
  });

  await waitFor("the pickle scan", async () => {
    const res = await ada.get(`${api}/repos/models/${modelRepo}/tree/main`);
    if (!res.ok()) return false;
    const items = (await res.json()).items as { path: string; scan_status?: string }[];
    return items.some((e) => e.path === "evil.pkl" && e.scan_status === "unsafe");
  });
  await waitFor("the Parquet conversion", async () => {
    const res = await ada.get(`${api}/repos/datasets/${datasetRepo}/configs`);
    if (!res.ok()) return false;
    const body = (await res.json()) as { configs: { splits: { status?: string }[] }[] };
    const splits = body.configs.flatMap((c) => c.splits);
    return splits.length > 0 && splits.every((s) => s.status === "ready");
  });
  await ada.dispose();
}
