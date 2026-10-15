import { expect } from "@playwright/test";
import {
  ensure,
  env,
  type Session,
  session,
  upstreamName,
  upstreamNamespace,
  user,
  waitFor,
} from "./helpers";

// Seeds a real Hangar for the e2e-real suite through its native API: the
// user ada, a model with a safetensors file and a pickle file the scan
// marks unsafe, a dataset with a CSV file the Viewer converts, a Collection
// and a discussion. It adds a second Instance as the Upstream
// "e2e-upstream", with a read Token as its Instance token, for the Discover
// and Pull flows, which make their own Repos on it. It waits for the scan
// and the conversion, so the flows see finished results. Reruns against the
// same Instances reuse what is there.

export { user };
export const modelRepo = "ada/tiny-model";
export const datasetRepo = "ada/tiny-data";
export const collection = { title: "E2E picks", slug: "e2e-picks" };

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

async function commitOnce(
  s: Session,
  type: "model" | "dataset",
  id: string,
  files: Record<string, Buffer>,
) {
  const [namespace, name] = id.split("/");
  const created = await s.call("POST", "/repos", { type, namespace, name, visibility: "public" });
  await ensure(created, `create ${id}`);
  if (created.status() === 409) return;
  const operations = Object.entries(files).map(([path, b]) => ({
    op: "add",
    path,
    content_base64: b.toString("base64"),
  }));
  await ensure(
    await s.call("POST", `/repos/${type}s/${id}/commits`, { summary: "Add files", operations }),
    `commit to ${id}`,
  );
}

/**
 * Makes the Organisation that holds the flows' GGUF Repos on the Upstream,
 * and adds the Upstream to the Instance under test with a new read Token
 * from it as the Instance token: the Upstream does not allow anonymous
 * reads, as Hangar's default is.
 */
async function seedUpstream(admin: Session) {
  const up = await session(
    env("HANGAR_E2E_UPSTREAM_URL"),
    "hangar-admin",
    env("HANGAR_E2E_UPSTREAM_ADMIN_PASSWORD"),
  );
  await ensure(
    await up.call("POST", "/orgs", { name: upstreamNamespace, display_name: "Lab" }),
    `create ${upstreamNamespace} on the Upstream`,
  );
  const token = await up.call("POST", "/me/tokens", {
    name: `e2e-instance-${Date.now()}`,
    scope: "read",
  });
  expect(token.status(), await token.text()).toBe(201);
  const secret = (await token.json()).secret as string;
  await up.dispose();

  const settings = {
    display_name: "Second Instance",
    base_url: env("HANGAR_E2E_UPSTREAM_URL"),
    enabled: true,
    instance_token: secret,
  };
  const created = await admin.call("POST", "/admin/upstreams", { name: upstreamName, ...settings });
  await ensure(created, `add the Upstream ${upstreamName}`);
  if (created.status() === 409) {
    const list = await admin.call("GET", "/admin/upstreams");
    const items = (await list.json()).items as { id: string; name: string }[];
    const existing = items.find((u) => u.name === upstreamName);
    if (!existing) throw new Error(`${upstreamName} answered 409 but is not listed`);
    await ensure(
      await admin.call("PATCH", `/admin/upstreams/${existing.id}`, settings),
      `update the Upstream ${upstreamName}`,
    );
  }
}

export default async function seed() {
  const baseURL = env("HANGAR_URL");
  const admin = await session(baseURL, "hangar-admin", env("HANGAR_E2E_ADMIN_PASSWORD"));
  await ensure(
    await admin.call("POST", "/admin/users", {
      ...user,
      email: "ada@example.com",
      display_name: "Ada Lovelace",
    }),
    "create ada",
  );
  // Reruns start with unsafe downloads allowed, the default.
  await ensure(
    await admin.call("PATCH", "/admin/settings", { scan_block_unsafe_downloads: false }),
    "reset settings",
  );
  await seedUpstream(admin);
  await admin.dispose();

  const ada = await session(baseURL, user.username, user.password);
  await commitOnce(ada, "model", modelRepo, {
    "README.md": Buffer.from(
      "---\nlicense: mit\n---\n# Tiny model\n\nA model for end-to-end tests.\n",
    ),
    "config.json": Buffer.from('{"model_type": "bert"}\n'),
    "model.safetensors": safetensors(),
    "evil.pkl": evilPickle,
  });
  await commitOnce(ada, "dataset", datasetRepo, {
    "README.md": Buffer.from("---\nlicense: cc-by-4.0\n---\n# Tiny data\n"),
    "data/train.csv": csv(),
  });
  // A Collection and a discussion, for the pages that show them.
  const col = await ada.call("GET", `/collections/ada/${collection.slug}`);
  if (col.status() === 404) {
    const made = await ada.call("POST", "/collections", {
      namespace: "ada",
      title: collection.title,
      visibility: "public",
    });
    expect(made.status(), await made.text()).toBe(201);
    expect((await made.json()).slug).toBe(collection.slug);
    await ensure(
      await ada.call("POST", `/collections/ada/${collection.slug}/items`, {
        type: "model",
        item_id: modelRepo,
        note: "Small enough for every test.",
      }),
      "add to the Collection",
    );
  } else {
    expect(col.status(), await col.text()).toBe(200);
  }
  const discussions = await ada.call("GET", `/repos/models/${modelRepo}/discussions`);
  expect(discussions.ok(), await discussions.text()).toBe(true);
  if ((await discussions.json()).total === 0) {
    const made = await ada.call("POST", `/repos/models/${modelRepo}/discussions`, {
      title: "Which tokenizer does this use?",
      description: "## Question\n\nThe card does not say.",
    });
    expect(made.status(), await made.text()).toBe(201);
  }

  await waitFor("the pickle scan", async () => {
    const res = await ada.call("GET", `/repos/models/${modelRepo}/tree/main`);
    if (!res.ok()) return false;
    const items = (await res.json()).items as { path: string; scan_status?: string }[];
    return items.some((e) => e.path === "evil.pkl" && e.scan_status === "unsafe");
  });
  await waitFor("the Parquet conversion", async () => {
    const res = await ada.call("GET", `/repos/datasets/${datasetRepo}/configs`);
    if (!res.ok()) return false;
    const body = (await res.json()) as { configs: { splits: { status?: string }[] }[] };
    const splits = body.configs.flatMap((c) => c.splits);
    return splits.length > 0 && splits.every((s) => s.status === "ready");
  });
  await ada.dispose();
}
