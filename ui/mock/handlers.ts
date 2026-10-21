import { randomBytes } from "node:crypto";
import type { components, operations } from "../src/api/schema";
import {
  iso,
  type MockCommit,
  type MockFile,
  type MockRepo,
  type MockState,
  randomUUID,
  sha1,
  uuidOf,
} from "./data.ts";
import { discoverDetail, discoverList } from "./discover.ts";
import {
  changesOf,
  dependenciesOf,
  fileOid,
  fileSize,
  findRepo,
  hasViewerFiles,
  lfsOid,
  listTree,
  quantGroups,
  resolveRev,
  treeEntry,
  unifiedDiff,
} from "./repo-model.ts";

type S = components["schemas"];
type Body = Record<string, unknown>;

export type Ctx = {
  state: MockState;
  /** The origin the request was sent to, standing in for server.public_url. */
  origin: string;
  params: Record<string, string>;
  query: URLSearchParams;
  body: unknown;
  raw?: Buffer;
};

export type Result = { status: number; body?: unknown; headers?: Record<string, string> };
export type Handler = (ctx: Ctx) => Result;

/** An RFC 9457 problem detail, as httpx.WriteProblem writes it. */
export function problem(status: number, detail: string, errors?: S["Problem"]["errors"]): Result {
  const titles: Record<number, string> = {
    400: "Bad Request",
    401: "Unauthorized",
    403: "Forbidden",
    404: "Not Found",
    409: "Conflict",
    412: "Precondition Failed",
    413: "Content Too Large",
    501: "Not Implemented",
  };
  return {
    status,
    body: {
      type: "about:blank",
      title: titles[status] ?? "Error",
      status,
      detail,
      request_id: randomUUID(),
      ...(errors ? { errors } : {}),
    },
  };
}

const ok = (body: unknown, status = 200): Result => ({ status, body });
const noContent: Result = { status: 204 };

function b(ctx: Ctx): Body {
  return (ctx.body ?? {}) as Body;
}

function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

function limitOf(ctx: Ctx, def = 50): number {
  const n = Number(ctx.query.get("limit") ?? def);
  return Number.isFinite(n) && n > 0 ? Math.min(n, 500) : def;
}

/** Cursor paging over an array: the cursor is the index to start at. */
function page<T>(ctx: Ctx, items: T[], def = 50): { items: T[]; next_cursor?: string } {
  const start = Number(ctx.query.get("cursor") ?? 0) || 0;
  const limit = limitOf(ctx, def);
  const slice = items.slice(start, start + limit);
  return start + limit < items.length
    ? { items: slice, next_cursor: String(start + limit) }
    : { items: slice };
}

function repoOr404(ctx: Ctx): MockRepo | Result {
  const r = findRepo(
    ctx.state,
    ctx.params.type ?? "models",
    ctx.params.ns ?? "",
    ctx.params.name ?? "",
  );
  if (!r)
    return problem(
      404,
      `Repo ${ctx.params.ns}/${ctx.params.name} does not exist, or you cannot see it.`,
    );
  return r;
}

function isResult(x: unknown): x is Result {
  return (
    typeof x === "object" &&
    x !== null &&
    typeof (x as { status?: unknown }).status === "number" &&
    !("repo" in x)
  );
}

function withRepo(fn: (ctx: Ctx, r: MockRepo) => Result): Handler {
  return (ctx) => {
    const r = repoOr404(ctx);
    return isResult(r) ? r : fn(ctx, r);
  };
}

function job(
  state: MockState,
  kind: string,
  payload: Body,
  status: S["JobStatus"] = "pending",
  result?: Body,
): S["Job"] {
  const j: S["Job"] = {
    id: randomUUID(),
    kind,
    status,
    attempts: status === "pending" ? 0 : 1,
    max_attempts: 5,
    payload,
    progress: { bytes_done: 0, bytes_total: 0, files_done: 0, files_total: 0, rate: 0 },
    created_by: state.me.username,
    created_at: new Date().toISOString(),
    ...(result ? { result, finished_at: new Date().toISOString() } : {}),
  };
  state.jobs.unshift(j);
  return j;
}

/** Moves running Pulls along with wall-clock time, so polling shows progress. */
export function tickJobs(state: MockState): void {
  const elapsed = (Date.now() - state.started) / 1000;
  for (const j of state.jobs) {
    if (j.status !== "running" || j.kind !== "pull" || !j.progress.bytes_total) continue;
    const start = (j.payload?.start_bytes as number | undefined) ?? j.progress.bytes_done;
    if (j.payload) j.payload.start_bytes = start;
    const done = Math.min(j.progress.bytes_total, start + elapsed * j.progress.rate);
    j.progress.bytes_done = Math.round(done);
    j.progress.eta_seconds = Math.round((j.progress.bytes_total - done) / j.progress.rate);
    for (const f of j.progress.files ?? [])
      f.bytes_done = Math.round(Math.min(f.bytes_total, done));
  }
}

function newCommit(
  r: MockRepo,
  parent: MockCommit,
  title: string,
  files: MockFile[],
  author: string,
  description?: string,
): MockCommit {
  const sha = sha1(`${r.repo.id}:${title}:${Date.now()}:${Math.random()}`);
  return {
    sha,
    parents: [parent.sha],
    title,
    message: description ? `${title}\n\n${description}` : title,
    author,
    date: new Date().toISOString(),
    files,
  };
}

function applyOps(ctx: Ctx, base: MockCommit, ops: S["CommitOperation"][]): MockFile[] | Result {
  const files = new Map(base.files.map((f) => [f.path, { ...f }]));
  for (const op of ops) {
    switch (op.op) {
      case "add": {
        const text = Buffer.from(op.content_base64 ?? "", "base64").toString("utf8");
        files.set(op.path, { path: op.path, text, scan: undefined });
        break;
      }
      case "add_lfs": {
        if (!op.sha256 || !ctx.state.lfsObjects.has(op.sha256)) {
          return problem(400, `The LFS object for ${op.path} has not been uploaded.`, [
            { pointer: "/operations", detail: `Upload ${op.path} before committing it.` },
          ]);
        }
        files.set(op.path, {
          path: op.path,
          size: op.size ?? 0,
          lfs: true,
          present: true,
          scan: "pending",
        });
        break;
      }
      case "delete":
        if (!files.delete(op.path)) return problem(404, `${op.path} does not exist.`);
        break;
      case "delete_folder":
        for (const p of [...files.keys()])
          if (p.startsWith(`${op.path.replace(/\/$/, "")}/`)) files.delete(p);
        break;
      case "copy": {
        const src = files.get(op.src_path ?? "");
        if (!src) return problem(404, `${op.src_path} does not exist.`);
        files.set(op.path, { ...src, path: op.path });
        break;
      }
    }
  }
  return [...files.values()];
}

function refreshSize(r: MockRepo): void {
  // Repo.sha is the head of the default branch, not the newest Commit on
  // any branch.
  const head = resolveRev(r, r.repo.default_branch) ?? (r.commits[0] as MockCommit);
  r.repo.sha = head.sha;
  r.repo.size_bytes = head.files.reduce((a, f) => a + fileSize(f), 0);
  r.repo.present_bytes = head.files.reduce(
    (a, f) => a + (f.present === false ? 0 : fileSize(f)),
    0,
  );
  r.repo.updated_at = head.date;
  r.repo.last_modified = head.date;
}

function listRepos(ctx: Ctx): Result {
  const q = ctx.query;
  const type = q.get("type");
  const formats = (q.get("format") ?? "").split(",").filter(Boolean);
  const visible = ctx.state.repos.filter((r) => !r.repo.deleted_at);
  const filtered = visible.filter((r) => {
    const s = r.repo.summary ?? {};
    const meta = (r.metadata ?? {}) as Body;
    if (type && `${r.repo.type}s` !== type) return false;
    const search = q.get("q");
    if (search && !r.repo.id.toLowerCase().includes(search.toLowerCase())) return false;
    if (q.get("ns") && r.repo.namespace !== q.get("ns")) return false;
    if (formats.length && !formats.every((f) => s.formats?.includes(f))) return false;
    if (q.get("task") && s.pipeline_tag !== q.get("task")) return false;
    if (q.get("library") && s.library_name !== q.get("library")) return false;
    if (q.get("quant") && !s.quants?.includes(q.get("quant") ?? "")) return false;
    if (q.get("licence") && r.repo.licence !== q.get("licence")) return false;
    if (q.get("mirror") && String(r.repo.mirror) !== q.get("mirror")) return false;
    if (q.get("params_min") && (s.parameters ?? 0) < Number(q.get("params_min"))) return false;
    if (
      q.get("params_max") &&
      (s.parameters ?? Number.POSITIVE_INFINITY) > Number(q.get("params_max"))
    )
      return false;
    if (q.get("context_min") && (s.context_length ?? 0) < Number(q.get("context_min")))
      return false;
    if (q.get("size_max") && r.repo.size_bytes > Number(q.get("size_max"))) return false;
    if (
      q.get("modality") &&
      !((meta.modalities as string[] | undefined) ?? []).includes(q.get("modality") ?? "")
    )
      return false;
    return true;
  });
  const sort = q.get("sort") ?? "updated";
  const key: Record<string, (r: MockRepo) => number | string> = {
    updated: (r) => -Date.parse(r.repo.updated_at),
    name: (r) => r.repo.id.toLowerCase(),
    size: (r) => -r.repo.size_bytes,
    parameters: (r) => -(r.repo.summary?.parameters ?? 0),
    downloads: (r) => -r.repo.downloads,
    likes: (r) => -r.repo.likes,
  };
  const k = key[sort] ?? (key.updated as (r: MockRepo) => number);
  filtered.sort((a, c) => {
    const x = k(a);
    const y = k(c);
    return x < y ? -1 : x > y ? 1 : 0;
  });
  const facets: Record<string, Record<string, number>> = {};
  const add = (name: string, v?: string) => {
    if (!v) return;
    const m = facets[name] ?? {};
    m[v] = (m[v] ?? 0) + 1;
    facets[name] = m;
  };
  // Facets count the Repos that match, as repo.List does.
  for (const r of filtered) {
    const s = r.repo.summary ?? {};
    for (const f of s.formats ?? []) add("format", f);
    for (const quant of s.quants ?? []) add("quant", quant);
    add("task", s.pipeline_tag);
    add("library", s.library_name);
    add("licence", r.repo.licence);
    add("mirror", String(r.repo.mirror));
    add("type", r.repo.type);
    add("namespace", r.repo.namespace);
    for (const m of (((r.metadata ?? {}) as Body).modalities as string[] | undefined) ?? [])
      add("modality", m);
  }
  return ok({
    ...page(
      ctx,
      filtered.map((r) => r.repo),
      50,
    ),
    facets,
  });
}

function discussionOr404(r: MockRepo, num: string | undefined): S["Discussion"] | Result {
  const d = r.discussions.find((x) => x.num === Number(num));
  return d ?? problem(404, `Discussion ${num} does not exist.`);
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

/** A Token secret as W5's NewTokenSecret makes it: hgr_ and 40 base62 characters. */
export function tokenSecret(): string {
  const bytes = randomBytes(40);
  return `hgr_${Array.from(bytes, (n) => base62[n % 62]).join("")}`;
}

/** W2's repo.PartSize: at least 16 MiB, and enough for 10,000 parts, rounded up to a MiB. */
export function partSizeFor(size: number): number {
  const mib = 1024 * 1024;
  const ps = Math.max(16 * mib, Math.ceil(size / 10_000));
  return Math.ceil(ps / mib) * mib;
}

/** W7's collections.Slugify: lower case, runs of other characters as one hyphen, at most 80. */
export function slugify(title: string): string {
  const s = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return s.length > 80 ? s.slice(0, 80).replace(/-+$/, "") : s;
}

/** Stores a single-use invite or reset link code and returns it. */
function newLink(state: MockState, kind: "invite" | "reset" | "email", username?: string): string {
  const code = randomBytes(24).toString("base64url");
  state.links[code] = kind;
  if (username) state.emailLinks[code] = username;
  return code;
}

export const handlers: Partial<Record<keyof operations, Handler>> = {
  // Session
  getMe: ({ state }) => (state.signedIn ? ok(state.me) : noContent),
  login: ({ state, body }) => {
    const { username, password } = body as { username: string; password: string };
    if (password === "wrong-password" || !password) {
      return problem(401, "Wrong username or password.");
    }
    state.signedIn = true;
    state.me = { ...state.me, username: username || state.me.username, has_password: true };
    return {
      status: 200,
      body: state.me,
      headers: { "Set-Cookie": "hangar_session=mock; Path=/; HttpOnly; SameSite=Lax" },
    };
  },
  logout: ({ state }) => {
    state.signedIn = false;
    return { status: 204, headers: { "Set-Cookie": "hangar_session=; Path=/; Max-Age=0" } };
  },
  listAuthProviders: ({ state }) =>
    ok({
      local_login: state.settings.local_login ?? true,
      registration: state.settings.registration ?? "closed",
      ui_external_images: state.settings.ui_external_images ?? true,
      providers: [{ id: "unifi", name: "UniFi", login_url: "/auth/oidc/unifi" }],
    }),
  updateMe: (ctx) => {
    const { state } = ctx;
    const p = b(ctx) as S["MeUpdate"];
    if (p.display_name !== undefined && p.display_name.length > 100) {
      return problem(422, "Invalid request: the display name can be at most 100 characters.", [
        { pointer: "/display_name", detail: "Use at most 100 characters." },
      ]);
    }
    if (p.email !== undefined) {
      const email = p.email.trim();
      if (email && !/^[^\s@]+@[^\s@]+$/.test(email)) {
        return problem(422, `Invalid request: "${email}" is not a valid email address.`);
      }
      const clash = state.users.find(
        (u) => u.username !== state.me.username && u.email?.toLowerCase() === email.toLowerCase(),
      );
      if (email && clash) {
        return problem(409, "Already exists: the email address is already used.");
      }
      // Like W5's UpdateProfile: a new address waits for its confirmation
      // link, the current one cancels a pending change, and an empty one
      // removes the email at once.
      if (!email) {
        state.me = { ...state.me, email: undefined, pending_email: undefined };
      } else if (email.toLowerCase() === state.me.email?.toLowerCase()) {
        state.me = { ...state.me, pending_email: undefined };
      } else {
        state.me = { ...state.me, pending_email: email };
      }
    }
    if (p.display_name !== undefined) {
      state.me = { ...state.me, display_name: p.display_name || state.me.username };
    }
    const self = state.users.find((u) => u.username === state.me.username);
    if (self) {
      Object.assign(self, {
        display_name: state.me.display_name,
        email: state.me.email,
        pending_email: state.me.pending_email,
      });
    }
    return ok(state.me);
  },
  changeMyPassword: (ctx) => {
    const { state } = ctx;
    const p = b(ctx) as S["PasswordChange"];
    if (!state.me.has_password) {
      return problem(
        403,
        "Forbidden: this account signs in with a sign-in provider and has no Hangar password; change your password with your provider.",
      );
    }
    if (p.current_password === "wrong-password" || !p.current_password) {
      return problem(403, "The current password is wrong.", [
        { pointer: "/current_password", detail: "The current password is wrong." },
      ]);
    }
    if ([...p.new_password].length < 10) {
      return problem(422, "Use at least 10 characters.", [
        { pointer: "/new_password", detail: "Use at least 10 characters." },
      ]);
    }
    state.sessions = state.sessions.filter((s) => s.current);
    return noContent;
  },
  listMySessions: ({ state }) => ok({ items: state.sessions }),
  revokeMySession: ({ state, params }) => {
    state.sessions = state.sessions.filter((s) => s.id !== params.id);
    return noContent;
  },

  // Tokens and keys
  listMyTokens: ({ state }) => ok({ items: state.tokens }),
  createMyToken: ({ state, body }) => {
    const t = body as S["TokenCreate"];
    if (state.tokens.some((x) => x.name === t.name)) {
      return problem(409, `You already have a Token called ${t.name}.`, [
        { pointer: "/name", detail: "Choose another name." },
      ]);
    }
    const secret = tokenSecret();
    const token: S["Token"] = {
      id: randomUUID(),
      name: t.name,
      prefix: secret.slice(0, 8),
      scope: t.scope,
      restrictions: t.restrictions,
      expires_at: t.expires_at,
      created_at: new Date().toISOString(),
    };
    state.tokens.unshift(token);
    state.audit.unshift({
      id: randomUUID(),
      action: "token.created",
      actor: state.me.username,
      target_type: "token",
      target_id: token.id,
      data: { name: t.name, scope: t.scope, user: state.me.username },
      created_at: token.created_at,
    });
    return ok({ ...token, secret }, 201);
  },
  revokeMyToken: ({ state, params }) => {
    state.tokens = state.tokens.filter((t) => t.id !== params.id);
    return noContent;
  },
  decideDeviceLogin: ({ body }) => {
    const d = body as S["DeviceDecision"];
    if (!/^[A-Z0-9]{4}-[A-Z0-9]{4}$/.test(d.user_code)) {
      return problem(
        404,
        "That code does not exist or has expired. Run the login command again for a new one.",
      );
    }
    return noContent;
  },
  listMyOllamaKeys: ({ state }) => ok({ items: state.ollamaKeys }),
  addMyOllamaKey: ({ state, body }) => {
    const k = body as S["OllamaKeyCreate"];
    if (!k.public_key.startsWith("ssh-ed25519 ")) {
      return problem(
        400,
        "That is not an Ollama public key. Paste the contents of ~/.ollama/id_ed25519.pub.",
        [{ pointer: "/public_key", detail: "Expected a line starting with ssh-ed25519." }],
      );
    }
    const key: S["OllamaKey"] = {
      id: randomUUID(),
      name: k.name,
      fingerprint: `SHA256:${Buffer.from(sha1(k.public_key)).toString("base64").slice(0, 43)}`,
      created_at: new Date().toISOString(),
    };
    state.ollamaKeys.push(key);
    return ok(key, 201);
  },
  deleteMyOllamaKey: ({ state, params }) => {
    state.ollamaKeys = state.ollamaKeys.filter((k) => k.id !== params.id);
    return noContent;
  },
  getMyUpstreamToken: ({ state, params }) => {
    const u = params.upstream ?? "";
    if (!state.upstreams.some((x) => x.name === u))
      return problem(404, `There is no Upstream called ${u}.`);
    return ok({
      upstream: u,
      saved: u in state.upstreamTokens,
      updated_at: state.upstreamTokens[u],
    });
  },
  setMyUpstreamToken: ({ state, params }) => {
    const u = params.upstream ?? "";
    state.upstreamTokens[u] = new Date().toISOString();
    return ok({ upstream: u, saved: true, updated_at: state.upstreamTokens[u] });
  },
  deleteMyUpstreamToken: ({ state, params }) => {
    delete state.upstreamTokens[params.upstream ?? ""];
    return noContent;
  },

  // Search and Arsenal
  search: ({ state, query }) => {
    const q = (query.get("q") ?? "").toLowerCase();
    return ok({
      repos: state.repos
        .filter((r) => r.repo.id.toLowerCase().includes(q))
        .slice(0, 8)
        .map((r) => r.repo),
      namespaces: state.namespaces.filter(
        (n) => n.name.toLowerCase().includes(q) || n.display_name.toLowerCase().includes(q),
      ),
      collections: state.collections.filter((c) => c.title.toLowerCase().includes(q)),
    });
  },
  listRepos,
  createRepo: ({ state, body }) => {
    const c = body as S["RepoCreate"];
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/.test(c.name)) {
      return problem(
        400,
        "Repo names use letters, digits, '-', '_' and '.', and start with a letter or digit.",
        [{ pointer: "/name", detail: "Use letters, digits, '-', '_' and '.'." }],
      );
    }
    if (findRepo(state, c.type, c.namespace, c.name)) {
      return problem(409, `${c.namespace}/${c.name} already exists.`, [
        { pointer: "/name", detail: "This name is taken." },
      ]);
    }
    const files: MockFile[] = c.readme
      ? [
          {
            path: "README.md",
            text: `---\nlicense: ${c.licence || "other"}\n---\n\n# ${c.name}\n\n${c.description ?? ""}\n`,
          },
        ]
      : [{ path: ".gitattributes", text: "*.safetensors filter=lfs diff=lfs merge=lfs -text\n" }];
    const sha = sha1(`${c.namespace}/${c.name}:init:${Date.now()}`);
    const created = new Date().toISOString();
    const mr: MockRepo = {
      repo: {
        id: `${c.namespace}/${c.name}`,
        uuid: randomUUID(),
        type: c.type,
        namespace: c.namespace,
        name: c.name,
        visibility: c.visibility ?? "public",
        description: c.description,
        licence: c.licence,
        default_branch: "main",
        mirror: false,
        gated: "",
        sha,
        created_at: created,
        updated_at: created,
        last_modified: created,
        likes: 0,
        downloads: 0,
        size_bytes: 0,
        lfs_bytes: 0,
        present_bytes: 0,
        tags: [],
        permissions: { read: true, write: true, admin: true, discuss: true, pull_mirror: true },
      },
      commits: [
        {
          sha,
          parents: [],
          title: "Initial commit",
          message: "Initial commit",
          author: state.me.username,
          date: created,
          files,
        },
      ],
      branches: { main: sha },
      tags: {},
      dependencies: { needs: [], needed_by: [] },
      discussions: [],
      collaborators: [],
      accessRequests: [],
      stats: { days: [], total: 0 },
      configs: c.type === "dataset" ? { commit: sha, from_card: false, configs: [] } : undefined,
    };
    refreshSize(mr);
    state.repos.unshift(mr);
    return ok(mr.repo, 201);
  },
  getRepo: withRepo((_c, r) => ok(r.repo)),
  updateRepo: withRepo((ctx, r) => {
    const u = b(ctx) as S["RepoUpdate"];
    if (u.default_branch && !r.branches[u.default_branch]) {
      return problem(400, `There is no branch called ${u.default_branch}.`, [
        { pointer: "/default_branch", detail: "Pick an existing branch." },
      ]);
    }
    if (u.description !== undefined) r.repo.description = u.description;
    if (u.visibility) r.repo.visibility = u.visibility;
    if (u.default_branch) r.repo.default_branch = u.default_branch;
    if (u.tags) r.repo.tags = u.tags;
    if (u.licence !== undefined) r.repo.licence = u.licence;
    if (u.gating) r.repo.gated = u.gating.mode ?? "";
    r.repo.updated_at = new Date().toISOString();
    return ok(r.repo);
  }),
  deleteRepo: withRepo((_c, r) => {
    r.repo.deleted_at = new Date().toISOString();
    return noContent;
  }),
  restoreRepo: (ctx) => {
    const r = ctx.state.repos.find(
      (x) => x.repo.id === `${ctx.params.ns}/${ctx.params.name}` && x.repo.deleted_at,
    );
    if (!r) return problem(404, "There is no deleted Repo with that ID.");
    r.repo.deleted_at = undefined;
    return ok(r.repo);
  },
  renameRepo: withRepo((ctx, r) => {
    const name = str(b(ctx).name) ?? "";
    if (findRepo(ctx.state, r.repo.type, r.repo.namespace, name))
      return problem(409, `${r.repo.namespace}/${name} already exists.`);
    r.repo.name = name;
    r.repo.id = `${r.repo.namespace}/${name}`;
    return ok(r.repo);
  }),
  transferRepo: withRepo((ctx, r) => {
    const ns = str(b(ctx).namespace) ?? "";
    if (!ctx.state.namespaces.some((n) => n.name === ns))
      return problem(404, `Namespace ${ns} does not exist.`);
    r.repo.namespace = ns;
    r.repo.id = `${ns}/${r.repo.name}`;
    return ok(r.repo);
  }),
  listRepoRefs: withRepo((_c, r) =>
    ok({
      branches: Object.entries(r.branches).map(([name, sha]) => ({
        name,
        ref: `refs/heads/${name}`,
        target_commit: sha,
      })),
      tags: Object.entries(r.tags).map(([name, sha]) => ({
        name,
        ref: `refs/tags/${name}`,
        target_commit: sha,
      })),
      converts: r.configs?.configs.some((c) => c.splits.some((s) => s.status === "ready"))
        ? [
            {
              name: "parquet",
              ref: "refs/convert/parquet",
              target_commit: sha1(`${r.repo.id}:parquet`),
            },
          ]
        : [],
      pull_requests: r.discussions
        .filter((d) => d.kind === "pull_request")
        .map((d) => ({
          name: `pr/${d.num}`,
          ref: `refs/pr/${d.num}`,
          target_commit: r.prHeads?.[d.num] ?? (r.commits[0] as MockCommit).sha,
        })),
    }),
  ),
  listCommits: withRepo((ctx, r) => {
    const head = resolveRev(r, ctx.query.get("rev") ?? undefined);
    if (!head) return problem(404, `Revision ${ctx.query.get("rev")} does not exist.`);
    const all = [...r.commits, ...(r.prCommits ?? [])];
    const chain: MockCommit[] = [];
    let c: MockCommit | undefined = head;
    while (c) {
      chain.push(c);
      c = c.parents[0] ? all.find((x) => x.sha === c?.parents[0]) : undefined;
    }
    const path = ctx.query.get("path");
    const items = chain
      .filter(
        (x) =>
          !path || changesOf(r, x).some((ch) => ch.path === path || ch.path.startsWith(`${path}/`)),
      )
      .map((x) => commitInfo(x));
    return ok(page(ctx, items, 50));
  }),
  createCommit: withRepo((ctx, r) => {
    const req = b(ctx) as S["CommitRequest"];
    if (r.repo.mirror)
      return problem(403, "Mirrors are read-only. Detach the Mirror to change its files.");
    const branch = req.branch || r.repo.default_branch;
    const base = resolveRev(r, branch);
    if (!base) return problem(404, `Branch ${branch} does not exist.`);
    if (req.parent_commit && req.parent_commit !== base.sha) {
      return problem(
        412,
        "The branch moved since you opened this file. Reload to see the newer version, then make your change again.",
      );
    }
    const files = applyOps(ctx, base, req.operations);
    if (isResult(files)) return files;
    const c = newCommit(r, base, req.summary, files, ctx.state.me.username, req.description);
    if (req.create_pr) {
      const num = Math.max(0, ...r.discussions.map((d) => d.num)) + 1;
      r.prCommits = [...(r.prCommits ?? []), c];
      r.prHeads = { ...(r.prHeads ?? {}), [num]: c.sha };
      r.discussions.push({
        num,
        kind: "pull_request",
        title: req.summary,
        status: "open",
        author: ctx.state.me.username,
        pr_ref: `refs/pr/${num}`,
        pr_base: r.repo.default_branch,
        created_at: c.date,
        events: req.description
          ? [
              {
                id: randomUUID(),
                type: "comment",
                author: ctx.state.me.username,
                body: req.description,
                created_at: c.date,
              },
            ]
          : [],
        changes: changesOf(r, c),
      });
      return ok({ sha: c.sha, ref: `refs/pr/${num}`, pull_request: num }, 201);
    }
    r.commits.unshift(c);
    r.branches[branch] = c.sha;
    refreshSize(r);
    return ok({ sha: c.sha, ref: `refs/heads/${branch}` }, 201);
  }),
  getCommit: withRepo((ctx, r) => {
    const c = resolveRev(r, ctx.params.sha);
    if (!c) return problem(404, `Commit ${ctx.params.sha} does not exist.`);
    return ok({ ...commitInfo(c), changes: changesOf(r, c) });
  }),
  getCommitFileDiff: withRepo((ctx, r) => {
    const c = resolveRev(r, ctx.params.sha);
    if (!c) return problem(404, `Commit ${ctx.params.sha} does not exist.`);
    const path = ctx.query.get("path") ?? "";
    const change = changesOf(r, c).find((x) => x.path === path);
    if (!change) return problem(404, `${path} did not change in this Commit.`);
    const all = [...r.commits, ...(r.prCommits ?? [])];
    const parent = all.find((x) => x.sha === c.parents[0]);
    const before = parent?.files.find((f) => f.path === path);
    const after = c.files.find((f) => f.path === path);
    const patch =
      change.binary || change.lfs ? "" : unifiedDiff(path, before?.text ?? "", after?.text ?? "");
    return ok({ ...change, patch });
  }),
  getPullRequestFileDiff: withRepo((ctx, r) => {
    const d = discussionOr404(r, ctx.params.num);
    if (isResult(d)) return d;
    if (d.kind !== "pull_request") return problem(422, "This discussion is not a pull request.");
    const prs = r.prCommits ?? [];
    const head = prs.find((c) => c.sha === r.prHeads?.[d.num]);
    if (!head) return problem(404, `${d.pr_ref} has no Commits yet.`);
    // The fork point: the first Commit on the way back that is not the pull request's.
    let fork: MockCommit | undefined = head;
    while (fork && prs.includes(fork)) {
      const parent: string | undefined = fork.parents[0];
      fork = [...r.commits, ...prs].find((x) => x.sha === parent);
    }
    const path = ctx.query.get("path") ?? "";
    const before = fork?.files.find((f) => f.path === path);
    const after = head.files.find((f) => f.path === path);
    if ((before?.text ?? before?.size) === (after?.text ?? after?.size)) {
      return problem(404, `${path} did not change in this pull request.`);
    }
    const f = (after ?? before) as MockFile;
    const lfs = Boolean(f.lfs);
    const binary = f.text === undefined;
    const change: S["FileChange"] = {
      path,
      status: !before ? "added" : !after ? "deleted" : "modified",
      old_oid: before ? fileOid(before) : undefined,
      new_oid: after ? fileOid(after) : undefined,
      old_size: before ? fileSize(before) : undefined,
      new_size: after ? fileSize(after) : undefined,
      lfs,
      binary,
    };
    const patch = binary || lfs ? "" : unifiedDiff(path, before?.text ?? "", after?.text ?? "");
    return ok({ ...change, patch });
  }),
  getTreeRoot: withRepo((ctx, r) => tree(ctx, r, "")),
  getTree: withRepo((ctx, r) => tree(ctx, r, ctx.params.path ?? "")),
  getRepoMetadata: withRepo((ctx, r) => {
    const c = resolveRev(r, ctx.query.get("rev") ?? undefined);
    if (!c) return problem(404, "Revision not found.");
    if (!r.metadata) return ok({ commit: c.sha, status: "pending" });
    return ok({ commit: c.sha, status: "ready", summary: r.metadata, indexed_at: c.date });
  }),
  getFileHeader: withRepo((ctx, r) => {
    const c = resolveRev(r, ctx.params.rev);
    const f = c?.files.find((x) => x.path === ctx.params.path);
    if (!f) return problem(404, `${ctx.params.path} does not exist at this Revision.`);
    if (!f.header) return problem(400, "Only safetensors and GGUF files have a header to show.");
    return ok(f.header);
  }),
  getFileScan: withRepo((ctx, r) => {
    const c = resolveRev(r, ctx.params.rev);
    const f = c?.files.find((x) => x.path === ctx.params.path);
    if (!f) return problem(404, `${ctx.params.path} does not exist at this Revision.`);
    return ok({ path: f.path, status: f.scan ?? "safe", findings: f.findings ?? [] });
  }),
  listQuants: withRepo((ctx, r) => {
    const c = resolveRev(r, ctx.query.get("rev") ?? undefined) as MockCommit;
    return ok({
      commit: c.sha,
      items: quantGroups(
        c.files.map((f) => ({ path: f.path, size: fileSize(f), present: f.present !== false })),
      ),
    });
  }),
  getDependencies: withRepo((ctx, r) => ok(dependenciesOf(ctx.state, r))),
  likeRepo: withRepo((_c, r) => {
    if (!r.repo.liked) r.repo.likes++;
    r.repo.liked = true;
    return ok({ liked: true, likes: r.repo.likes });
  }),
  unlikeRepo: withRepo((_c, r) => {
    if (r.repo.liked) r.repo.likes--;
    r.repo.liked = false;
    return ok({ liked: false, likes: r.repo.likes });
  }),
  getRepoStats: withRepo((_c, r) => ok(r.stats)),
  listCollaborators: withRepo((_c, r) => ok({ items: r.collaborators })),
  setCollaborators: withRepo((ctx, r) => {
    const items = (b(ctx).items as S["Collaborator"][]) ?? [];
    const unknown = items.find((i) => !ctx.state.users.some((u) => u.username === i.username));
    if (unknown) return problem(404, `There is no user called ${unknown.username}.`);
    r.collaborators = items;
    return ok({ items });
  }),
  listAccessRequests: withRepo((ctx, r) => {
    const status = ctx.query.get("status");
    return ok(
      page(
        ctx,
        r.accessRequests.filter((a) => !status || a.status === status),
      ),
    );
  }),
  requestAccess: withRepo((ctx, r) => {
    const a: S["AccessRequest"] = {
      id: randomUUID(),
      username: ctx.state.me.username,
      status: r.repo.gated === "auto" ? "accepted" : "pending",
      fields: (b(ctx).fields as Body) ?? {},
      created_at: new Date().toISOString(),
    };
    r.accessRequests.push(a);
    return ok(a, 201);
  }),
  decideAccessRequest: withRepo((ctx, r) => {
    const a = r.accessRequests.find((x) => x.id === ctx.params.id);
    if (!a) return problem(404, "That access request does not exist.");
    a.status = b(ctx).status as S["AccessRequestStatus"];
    a.decided_at = new Date().toISOString();
    a.decided_by = ctx.state.me.username;
    return ok(a);
  }),

  // Uploads
  planUploads: withRepo((ctx, r) => {
    const req = b(ctx) as S["UploadPlanRequest"];
    if (r.repo.mirror)
      return problem(403, "Mirrors are read-only. Detach the Mirror to upload files.");
    const lfsExt =
      /\.(safetensors|bin|gguf|pt|pth|ckpt|onnx|parquet|arrow|h5|msgpack|zip|tar|gz|png|jpg|wav|mp3|npy|pkl)$/i;
    const files = req.files.map((f): S["UploadPlanItem"] => {
      if (!lfsExt.test(f.path) && f.size <= 10 * 1024 * 1024)
        return { path: f.path, action: "inline" };
      if (ctx.state.lfsObjects.has(f.sha256)) return { path: f.path, action: "exists" };
      // Like W2's PlanUploads, a plan continues an upload of the same
      // object in this Repo that is still in progress, so its parts are
      // kept, and part URLs are absolute (built from server.public_url).
      const partSize = partSizeFor(f.size);
      const count = Math.max(1, Math.ceil(f.size / partSize));
      const inProgress = Object.entries(ctx.state.uploads).find(
        ([, u]) =>
          u.sha256 === f.sha256 &&
          u.size === f.size &&
          u.partSize === partSize &&
          u.repo === r.repo.id &&
          u.state === "uploading",
      );
      const id = inProgress?.[0] ?? randomUUID();
      if (!inProgress) {
        ctx.state.uploads[id] = {
          sha256: f.sha256,
          size: f.size,
          partSize,
          parts: [],
          state: "uploading",
          repo: r.repo.id,
        };
      }
      const base = `${ctx.origin}/api/hangar/v1/repos/${ctx.params.type}/${r.repo.namespace}/${r.repo.name}/uploads/${id}/parts`;
      return {
        path: f.path,
        action: "upload",
        upload_id: id,
        part_size: partSize,
        part_urls: Array.from({ length: count }, (_, i) => `${base}/${i + 1}`),
        presigned: false,
      };
    });
    return ok({ files });
  }),
  getUpload: (ctx) => {
    const u = ctx.state.uploads[ctx.params.id ?? ""];
    if (!u) return problem(404, "That upload does not exist or has expired.");
    return ok(uploadStatus(ctx.params.id ?? "", u));
  },
  putUploadPart: (ctx) => {
    const u = ctx.state.uploads[ctx.params.id ?? ""];
    if (!u) return problem(404, "That upload does not exist or has expired.");
    const n = Number(ctx.params.n);
    const size = ctx.raw?.length ?? 0;
    const etag = `"${sha1(`${ctx.params.id}:${n}:${size}`)}"`;
    u.parts = [...u.parts.filter((p) => p.number !== n), { number: n, etag, size }].sort(
      (a, c) => a.number - c.number,
    );
    return { status: 200, body: { number: n, etag, size }, headers: { ETag: etag } };
  },
  completeUpload: (ctx) => {
    const u = ctx.state.uploads[ctx.params.id ?? ""];
    if (!u) return problem(404, "That upload does not exist or has expired.");
    const parts = (b(ctx).parts as S["UploadedPart"][]) ?? [];
    const total = u.parts.reduce((a, p) => a + (p.size ?? 0), 0);
    if (parts.length !== Math.max(1, Math.ceil(u.size / u.partSize)) || total !== u.size) {
      return problem(400, "Some parts are missing or the wrong size. Upload them again.");
    }
    u.state = "verified";
    ctx.state.lfsObjects.add(u.sha256);
    return ok(uploadStatus(ctx.params.id ?? "", u));
  },
  startZipExport: withRepo((ctx, r) => {
    const rev = ctx.query.get("rev") ?? r.repo.default_branch;
    const c = resolveRev(r, rev);
    if (!c) return problem(404, `Revision ${rev} does not exist.`);
    const path = ctx.query.get("path") ?? "";
    const filename = `${r.repo.name}-${rev.replaceAll("/", "-")}${path ? `-${path.replaceAll("/", "-")}` : ""}.zip`;
    const j = job(ctx.state, "zip.export", { repo: r.repo.id, rev, path }, "succeeded", {
      key: `exports/${randomUUID()}.zip`,
      filename,
      size: 1234,
      files: c.files.length,
      url: `/api/hangar/v1/exports/${encodeURIComponent(filename)}?sig=mock`,
    });
    return ok(j, 202);
  }),

  // Datasets
  listDatasetConfigs: (ctx) => {
    const r = findRepo(ctx.state, "datasets", ctx.params.ns ?? "", ctx.params.name ?? "");
    if (!r) return problem(404, "Dataset not found.");
    if (!r.configs?.configs.length && !hasViewerFiles(r)) {
      // As internal/api/datasets.go answers for layout.ErrEmptyDataset.
      const p = problem(
        422,
        "Hangar cannot read this dataset's Configs and Splits: The dataset is empty.",
      );
      return { ...p, body: { ...(p.body as Body), title: "The dataset has no Configs" } };
    }
    return ok(r.configs ?? { commit: r.commits[0]?.sha ?? "", from_card: false, configs: [] });
  },

  // Mirrors
  getMirror: withRepo((_c, r) =>
    r.mirror ? ok(r.mirror) : problem(404, "This Repo is not a Mirror."),
  ),
  updateMirror: withRepo((ctx, r) => {
    if (!r.mirror) return problem(404, "This Repo is not a Mirror.");
    const u = b(ctx) as S["MirrorUpdate"];
    r.mirror = { ...r.mirror, ...u, selection: u.selection ?? r.mirror.selection };
    return ok(r.mirror);
  }),
  syncMirror: withRepo((ctx, r) => {
    if (!r.mirror) return problem(404, "This Repo is not a Mirror.");
    return ok(job(ctx.state, "mirror.sync", { repo: r.repo.id }, "pending"), 202);
  }),
  detachMirror: withRepo((_c, r) => {
    if (!r.mirror) return problem(404, "This Repo is not a Mirror.");
    r.mirror = undefined;
    r.repo.mirror = false;
    r.repo.mirror_status = undefined;
    return ok(r.repo);
  }),
  pullMoreIntoMirror: withRepo((ctx, r) => {
    const sel = b(ctx) as S["Selection"];
    const j = job(ctx.state, "pull", { repo: r.repo.id, selection: sel }, "running");
    j.progress = {
      bytes_done: 0,
      bytes_total: 100 * 1024 * 1024,
      files_done: 0,
      files_total: sel.paths?.length ?? 1,
      rate: 8 * 1024 * 1024,
    };
    return ok(j, 202);
  }),

  // Discover and Pulls
  listUpstreams: ({ state }) => ok({ items: state.upstreams.filter((u) => u.enabled) }),
  discover: (ctx) => {
    const items = discoverList(
      ctx.state,
      ctx.params.type ?? "models",
      ctx.query.get("search") ?? "",
      ctx.query.getAll("filter"),
    );
    return ok(page(ctx, items, 30));
  },
  getDiscoverDetail: (ctx) => {
    const d = discoverDetail(
      ctx.state,
      ctx.params.type ?? "models",
      ctx.params.ns ?? "",
      ctx.params.name ?? "",
    );
    return d
      ? ok(d)
      : problem(404, `${ctx.params.ns}/${ctx.params.name} was not found on the Upstream.`);
  },
  startPull: (ctx) => {
    const p = b(ctx) as S["PullCreate"];
    const detail = discoverDetail(
      ctx.state,
      `${p.type}s`,
      ...(p.repo.split("/") as [string, string]),
    );
    const total = detail
      ? detail.files
          .filter(
            (f) =>
              !p.selection?.quants?.length ||
              !f.path.endsWith(".gguf") ||
              p.selection.quants.some((q) => f.path.toUpperCase().includes(q)),
          )
          .reduce((a, f) => a + f.size, 0)
      : 0;
    const j = job(
      ctx.state,
      "pull",
      { repo: p.repo, upstream: p.upstream, type: p.type, selection: p.selection ?? {} },
      "running",
    );
    j.progress = {
      bytes_done: 0,
      bytes_total: total,
      files_done: 0,
      files_total: detail?.files.length ?? 0,
      rate: 12 * 1024 * 1024,
      eta_seconds: Math.round(total / (12 * 1024 * 1024)),
    };
    j.started_at = j.created_at;
    return ok(j, 202);
  },
  getPull: (ctx) => {
    const j = ctx.state.jobs.find((x) => x.id === ctx.params.id && x.kind === "pull");
    return j ? ok(j) : problem(404, "That Pull does not exist.");
  },

  // Jobs
  listJobs: (ctx) => {
    tickJobs(ctx.state);
    const kind = ctx.query.get("kind");
    const status = ctx.query.get("status");
    const mine = ctx.query.get("mine") === "true";
    return ok(
      page(
        ctx,
        ctx.state.jobs.filter(
          (j) =>
            (!kind || j.kind === kind) &&
            (!status || j.status === status) &&
            (!mine || j.created_by === ctx.state.me.username),
        ),
      ),
    );
  },
  getJob: (ctx) => {
    tickJobs(ctx.state);
    const j = ctx.state.jobs.find((x) => x.id === ctx.params.id);
    return j ? ok(j) : problem(404, "That Job does not exist.");
  },
  cancelJob: (ctx) => {
    const j = ctx.state.jobs.find((x) => x.id === ctx.params.id);
    if (!j) return problem(404, "That Job does not exist.");
    if (j.status !== "pending" && j.status !== "running")
      return problem(409, "Only waiting or running Jobs can be cancelled.");
    j.status = "cancelled";
    j.finished_at = new Date().toISOString();
    return ok(j);
  },
  retryJob: (ctx) => {
    const j = ctx.state.jobs.find((x) => x.id === ctx.params.id);
    if (!j) return problem(404, "That Job does not exist.");
    if (j.status !== "failed" && j.status !== "cancelled")
      return problem(409, "Only failed or cancelled Jobs can be retried.");
    j.status = "pending";
    j.error = undefined;
    j.finished_at = undefined;
    return ok(j, 202);
  },

  // Namespaces
  getNamespace: (ctx) => {
    const n = ctx.state.namespaces.find(
      (x) => x.name.toLowerCase() === (ctx.params.ns ?? "").toLowerCase(),
    );
    if (!n) return problem(404, `Namespace ${ctx.params.ns} does not exist.`);
    return ok({
      ...n,
      repo_count: ctx.state.repos.filter((r) => r.repo.namespace === n.name).length,
    });
  },
  createOrg: (ctx) => {
    const o = b(ctx) as S["OrgCreate"];
    if (ctx.state.namespaces.some((n) => n.name.toLowerCase() === o.name.toLowerCase())) {
      return problem(409, `The name ${o.name} is taken.`, [
        { pointer: "/name", detail: "Choose another name." },
      ]);
    }
    const n: S["Namespace"] = {
      name: o.name,
      kind: "org",
      display_name: o.display_name || o.name,
      description: o.description,
      member_count: 1,
      created_at: new Date().toISOString(),
    };
    ctx.state.namespaces.push(n);
    ctx.state.members[o.name] = [
      {
        username: ctx.state.me.username,
        display_name: ctx.state.me.display_name,
        role: "admin",
        source: "manual",
      },
    ];
    ctx.state.me.orgs.push({ name: o.name, display_name: n.display_name, role: "admin" });
    return ok(n, 201);
  },
  updateOrg: (ctx) => {
    const n = ctx.state.namespaces.find((x) => x.name === ctx.params.ns && x.kind === "org");
    if (!n) return problem(404, "Organisation not found.");
    Object.assign(n, b(ctx));
    return ok(n);
  },
  deleteOrg: (ctx) => {
    if (ctx.state.repos.some((r) => r.repo.namespace === ctx.params.ns))
      return problem(409, "Move or delete this Organisation's Repos first.");
    ctx.state.namespaces = ctx.state.namespaces.filter((x) => x.name !== ctx.params.ns);
    return noContent;
  },
  listMembers: (ctx) => ok({ items: ctx.state.members[ctx.params.ns ?? ""] ?? [] }),
  getMember: (ctx) => {
    const m = (ctx.state.members[ctx.params.ns ?? ""] ?? []).find(
      (x) => x.username === ctx.params.user,
    );
    return m ? ok(m) : problem(404, "Not a Member.");
  },
  setMember: (ctx) => {
    const ns = ctx.params.ns ?? "";
    const user = ctx.state.users.find((u) => u.username === ctx.params.user);
    if (!user) return problem(404, `There is no user called ${ctx.params.user}.`);
    const list = ctx.state.members[ns] ?? [];
    const role = b(ctx).role as S["Role"];
    const admins = list.filter((m) => m.role === "admin");
    if (admins.length === 1 && admins[0]?.username === user.username && role !== "admin") {
      return problem(
        409,
        "An Organisation needs at least one admin. Make someone else an admin first.",
      );
    }
    const m: S["Member"] = {
      username: user.username,
      display_name: user.display_name,
      role,
      source: "manual",
    };
    ctx.state.members[ns] = [...list.filter((x) => x.username !== user.username), m];
    return ok(m);
  },
  removeMember: (ctx) => {
    const ns = ctx.params.ns ?? "";
    const list = ctx.state.members[ns] ?? [];
    const admins = list.filter((m) => m.role === "admin");
    if (admins.length === 1 && admins[0]?.username === ctx.params.user) {
      return problem(
        409,
        "An Organisation needs at least one admin. Make someone else an admin first.",
      );
    }
    ctx.state.members[ns] = list.filter((x) => x.username !== ctx.params.user);
    return noContent;
  },

  // Collections
  listCollections: (ctx) => {
    const owner = ctx.query.get("owner");
    const item = ctx.query.get("item");
    const q = ctx.query.get("q")?.toLowerCase();
    return ok({
      items: ctx.state.collections
        .filter((c) => !owner || c.namespace === owner)
        .filter((c) => !q || c.title.toLowerCase().includes(q))
        .filter((c) => !item || c.items.some((i) => `${i.type}s/${i.item_id}` === item))
        .map(expandCollection(ctx.state)),
    });
  },
  createCollection: (ctx) => {
    const c = b(ctx) as S["CollectionCreate"];
    const slug = slugify(c.title.trim());
    if (!slug) return problem(400, "The title needs a letter or digit.");
    if (ctx.state.collections.some((x) => x.namespace === c.namespace && x.slug === slug)) {
      return problem(409, `A Collection ${c.namespace}/${slug} already exists.`);
    }
    const col: S["Collection"] = {
      namespace: c.namespace,
      slug,
      title: c.title,
      description: c.description,
      visibility: c.visibility ?? "public",
      items: [],
      updated_at: new Date().toISOString(),
    };
    ctx.state.collections.push(col);
    return ok(col, 201);
  },
  getCollection: (ctx) => {
    const c = findCollection(ctx);
    return c ? ok(expandCollection(ctx.state)(c)) : problem(404, "Collection not found.");
  },
  updateCollection: (ctx) => {
    const c = findCollection(ctx);
    if (!c) return problem(404, "Collection not found.");
    Object.assign(c, b(ctx), { updated_at: new Date().toISOString() });
    return ok(expandCollection(ctx.state)(c));
  },
  deleteCollection: (ctx) => {
    ctx.state.collections = ctx.state.collections.filter(
      (c) => !(c.namespace === ctx.params.ns && c.slug === ctx.params.slug),
    );
    return noContent;
  },
  addCollectionItem: (ctx) => {
    const c = findCollection(ctx);
    if (!c) return problem(404, "Collection not found.");
    const i = b(ctx) as S["CollectionItemCreate"];
    if (
      i.type !== "collection" &&
      !findRepo(ctx.state, i.type, ...(i.item_id.split("/") as [string, string]))
    ) {
      return problem(404, `${i.item_id} is not in the arsenal.`, [
        { pointer: "/item_id", detail: "Pick a Repo you can see." },
      ]);
    }
    if (c.items.some((x) => x.item_id === i.item_id && x.type === i.type))
      return problem(409, `${i.item_id} is already in this Collection.`);
    c.items.push({
      id: randomUUID(),
      type: i.type,
      item_id: i.item_id,
      note: i.note,
      position: c.items.length,
    });
    return ok(expandCollection(ctx.state)(c));
  },
  updateCollectionItem: (ctx) => {
    const c = findCollection(ctx);
    const item = c?.items.find((x) => x.id === ctx.params.id);
    if (!c || !item) return problem(404, "Item not found.");
    const u = b(ctx) as S["CollectionItemUpdate"];
    if (u.note !== undefined) item.note = u.note;
    if (u.position !== undefined) {
      const rest = c.items.filter((x) => x.id !== item.id).sort((x, y) => x.position - y.position);
      rest.splice(Math.min(u.position, rest.length), 0, item);
      rest.forEach((x, i) => {
        x.position = i;
      });
      c.items = rest;
    }
    return ok(expandCollection(ctx.state)(c));
  },
  deleteCollectionItem: (ctx) => {
    const c = findCollection(ctx);
    if (!c) return problem(404, "Collection not found.");
    c.items = c.items.filter((x) => x.id !== ctx.params.id).map((x, i) => ({ ...x, position: i }));
    return noContent;
  },

  // Community
  listDiscussions: withRepo((ctx, r) => {
    const kind = ctx.query.get("kind");
    const status = ctx.query.get("status");
    const items = r.discussions
      .filter(
        (d) =>
          (!kind || d.kind === kind) &&
          (!status ||
            (status === "open"
              ? d.status === "open" || d.status === "draft"
              : d.status !== "open")),
      )
      .sort((a, c) => c.num - a.num)
      .map(({ events, changes, ...d }) => ({ ...d }));
    return ok({ ...page(ctx, items), total: items.length });
  }),
  createDiscussion: withRepo((ctx, r) => {
    const c = b(ctx) as S["DiscussionCreate"];
    const num = Math.max(0, ...r.discussions.map((d) => d.num)) + 1;
    const now = new Date().toISOString();
    const d: S["Discussion"] = {
      num,
      kind: c.pull_request ? "pull_request" : "discussion",
      title: c.title,
      status: c.pull_request ? "draft" : "open",
      author: ctx.state.me.username,
      created_at: now,
      events: c.description
        ? [
            {
              id: randomUUID(),
              type: "comment",
              author: ctx.state.me.username,
              body: c.description,
              created_at: now,
            },
          ]
        : [],
      ...(c.pull_request
        ? { pr_ref: `refs/pr/${num}`, pr_base: c.base ?? r.repo.default_branch, changes: [] }
        : {}),
    };
    r.discussions.push(d);
    return ok(d, 201);
  }),
  getDiscussion: withRepo((ctx, r) => {
    const d = discussionOr404(r, ctx.params.num);
    return isResult(d) ? d : ok(d);
  }),
  updateDiscussion: withRepo((ctx, r) => {
    const d = discussionOr404(r, ctx.params.num);
    if (isResult(d)) return d;
    const u = b(ctx) as S["DiscussionUpdate"];
    const now = new Date().toISOString();
    if (u.title && u.title !== d.title) {
      d.events = [
        ...(d.events ?? []),
        {
          id: randomUUID(),
          type: "title-change",
          author: ctx.state.me.username,
          data: { from: d.title, to: u.title },
          created_at: now,
        },
      ];
      d.title = u.title;
    }
    if (u.status && u.status !== d.status) {
      d.status = u.status;
      d.events = [
        ...(d.events ?? []),
        {
          id: randomUUID(),
          type: "status-change",
          author: ctx.state.me.username,
          data: { status: u.status },
          body: u.comment,
          created_at: now,
        },
      ];
    }
    return ok(d);
  }),
  commentOnDiscussion: withRepo((ctx, r) => {
    const d = discussionOr404(r, ctx.params.num);
    if (isResult(d)) return d;
    const e: S["DiscussionEvent"] = {
      id: randomUUID(),
      type: "comment",
      author: ctx.state.me.username,
      body: str(b(ctx).body),
      created_at: new Date().toISOString(),
    };
    d.events = [...(d.events ?? []), e];
    return ok(e, 201);
  }),
  mergePullRequest: withRepo((ctx, r) => {
    const d = discussionOr404(r, ctx.params.num);
    if (isResult(d)) return d;
    if (d.kind !== "pull_request") return problem(400, "Only pull requests can be merged.");
    if (d.status !== "open") return problem(409, "Only open pull requests can be merged.");
    const head = r.prCommits?.find((c) => c.sha === r.prHeads?.[d.num]);
    if (head) {
      r.commits.unshift({ ...head, parents: [(r.commits[0] as MockCommit).sha] });
      r.branches[r.repo.default_branch] = head.sha;
      refreshSize(r);
    }
    d.status = "merged";
    d.events = [
      ...(d.events ?? []),
      {
        id: randomUUID(),
        type: "status-change",
        author: ctx.state.me.username,
        data: { status: "merged" },
        body: str(b(ctx).comment),
        created_at: new Date().toISOString(),
      },
    ];
    return ok(d);
  }),

  // Webhooks
  listWebhooks: (ctx) => {
    const ns = ctx.query.get("ns");
    const repo = ctx.query.get("repo");
    return ok({
      items: ctx.state.webhooks.filter(
        (w) => (!ns || w.namespace === ns) && (!repo || w.repo === repo),
      ),
    });
  },
  createWebhook: (ctx) => {
    const w = b(ctx) as S["WebhookCreate"];
    if (!/^https?:\/\//.test(w.url))
      return problem(400, "Webhook URLs start with https:// or http://.", [
        { pointer: "/url", detail: "Enter a full URL." },
      ]);
    const hook: S["Webhook"] = {
      id: randomUUID(),
      namespace: w.namespace,
      repo: w.repo,
      url: w.url,
      events: w.events,
      enabled: w.enabled ?? true,
      created_at: new Date().toISOString(),
    };
    ctx.state.webhooks.push(hook);
    ctx.state.deliveries[hook.id] = [];
    return ok(hook, 201);
  },
  getWebhook: (ctx) => {
    const w = ctx.state.webhooks.find((x) => x.id === ctx.params.id);
    return w ? ok(w) : problem(404, "Webhook not found.");
  },
  updateWebhook: (ctx) => {
    const w = ctx.state.webhooks.find((x) => x.id === ctx.params.id);
    if (!w) return problem(404, "Webhook not found.");
    const { secret: _secret, ...rest } = b(ctx);
    Object.assign(w, rest);
    return ok(w);
  },
  deleteWebhook: (ctx) => {
    ctx.state.webhooks = ctx.state.webhooks.filter((x) => x.id !== ctx.params.id);
    return noContent;
  },
  listWebhookDeliveries: (ctx) => ok(page(ctx, ctx.state.deliveries[ctx.params.id ?? ""] ?? [])),
  redeliverWebhook: (ctx) => {
    const list = ctx.state.deliveries[ctx.params.id ?? ""] ?? [];
    const d = list.find((x) => x.id === ctx.params.delivery_id);
    if (!d) return problem(404, "Delivery not found.");
    const again: S["WebhookDelivery"] = {
      ...d,
      id: randomUUID(),
      attempt: 1,
      status_code: undefined,
      error: undefined,
      delivered_at: undefined,
      created_at: new Date().toISOString(),
    };
    list.unshift(again);
    return ok(again, 202);
  },
  testWebhook: (ctx) => {
    const list = ctx.state.deliveries[ctx.params.id ?? ""];
    if (!list) return problem(404, "Webhook not found.");
    const d: S["WebhookDelivery"] = {
      id: randomUUID(),
      event: "ping",
      attempt: 1,
      created_at: new Date().toISOString(),
      payload: '{"event":"ping"}',
    };
    list.unshift(d);
    return ok(d, 202);
  },

  // Admin
  adminListOrgs: (ctx) => {
    const q = ctx.query.get("q")?.toLowerCase();
    return ok(
      page(
        ctx,
        ctx.state.namespaces
          .filter((n) => n.kind === "org")
          .filter(
            (n) =>
              !q || n.name.toLowerCase().includes(q) || n.display_name.toLowerCase().includes(q),
          )
          .sort((a, c) => a.name.toLowerCase().localeCompare(c.name.toLowerCase())),
      ),
    );
  },
  adminListUsers: (ctx) => {
    const q = ctx.query.get("q")?.toLowerCase();
    return ok(
      page(
        ctx,
        ctx.state.users.filter((u) => !q || u.username.includes(q) || (u.email ?? "").includes(q)),
      ),
    );
  },
  adminCreateUser: (ctx) => {
    const c = b(ctx) as S["AdminUserCreate"];
    if (
      ctx.state.users.some((u) => u.username === c.username) ||
      ctx.state.namespaces.some((n) => n.name === c.username)
    ) {
      return problem(409, `The name ${c.username} is taken.`, [
        { pointer: "/username", detail: "Choose another username." },
      ]);
    }
    if (c.password !== undefined && c.password.length < 10) {
      return problem(400, "Passwords need at least 10 characters.", [
        { pointer: "/password", detail: "Use at least 10 characters." },
      ]);
    }
    const u: S["AdminUser"] = {
      id: randomUUID(),
      username: c.username,
      email: c.email,
      display_name: c.display_name,
      is_admin: Boolean(c.is_admin),
      disabled: false,
      created_at: new Date().toISOString(),
    };
    ctx.state.users.push(u);
    ctx.state.namespaces.push({
      name: c.username,
      kind: "user",
      display_name: c.display_name || c.username,
    });
    return ok(u, 201);
  },
  adminUpdateUser: (ctx) => {
    const u = ctx.state.users.find((x) => x.id === ctx.params.id);
    if (!u) return problem(404, "User not found.");
    const p = b(ctx) as S["AdminUserUpdate"];
    const enabledAdmins = ctx.state.users.filter((x) => x.is_admin && !x.disabled);
    if (u.is_admin && enabledAdmins.length === 1 && (p.disabled === true || p.is_admin === false)) {
      return problem(409, "This is the last enabled admin. Make another user an admin first.");
    }
    Object.assign(u, p);
    return ok(u);
  },
  adminCreateResetLink: (ctx) => {
    const u = ctx.state.users.find((x) => x.id === ctx.params.id);
    if (!u) return problem(404, "User not found.");
    return ok(
      {
        url: `${ctx.origin}/auth/reset/${newLink(ctx.state, "reset")}`,
        expires_at: iso(-72 * 3_600_000),
      },
      201,
    );
  },
  adminCreateEmailLink: (ctx) => {
    const u = ctx.state.users.find((x) => x.id === ctx.params.id);
    if (!u) return problem(404, "User not found.");
    if (!u.pending_email) {
      return problem(
        422,
        "Invalid request: this user has no email change waiting for confirmation.",
      );
    }
    // Like W5's CreateEmailLink: bound to the address the admin saw.
    if ((b(ctx) as S["EmailLinkCreate"]).email !== u.pending_email) {
      return problem(
        409,
        "Already exists: the user's pending email has changed since you looked; reload and check the new address.",
      );
    }
    return ok(
      {
        url: `${ctx.origin}/auth/email/${newLink(ctx.state, "email", u.username)}`,
        expires_at: iso(-72 * 3_600_000),
        email: u.pending_email,
      },
      201,
    );
  },
  adminCreateInvite: (ctx) =>
    ok(
      {
        url: `${ctx.origin}/auth/invite/${newLink(ctx.state, "invite")}`,
        expires_at: iso(-72 * 3_600_000),
      },
      201,
    ),
  adminListUpstreams: ({ state }) => ok({ items: state.upstreams }),
  adminCreateUpstream: (ctx) => {
    const c = b(ctx) as S["UpstreamCreate"];
    if (ctx.state.upstreams.some((u) => u.name === c.name))
      return problem(409, `An Upstream called ${c.name} exists.`, [
        { pointer: "/name", detail: "Choose another name." },
      ]);
    const u: S["Upstream"] = {
      id: randomUUID(),
      name: c.name,
      display_name: c.display_name || c.name,
      base_url: c.base_url,
      viewer_url: c.viewer_url,
      enabled: c.enabled ?? true,
      namespace_prefix: c.namespace_prefix,
      source: "admin",
      has_instance_token: Boolean(c.instance_token),
    };
    ctx.state.upstreams.push(u);
    return ok(u, 201);
  },
  adminGetUpstream: (ctx) => {
    const u = ctx.state.upstreams.find((x) => x.id === ctx.params.id);
    return u ? ok(u) : problem(404, "Upstream not found.");
  },
  adminUpdateUpstream: (ctx) => {
    const u = ctx.state.upstreams.find((x) => x.id === ctx.params.id);
    if (!u) return problem(404, "Upstream not found.");
    const { instance_token, ...rest } = b(ctx) as S["UpstreamUpdate"];
    Object.assign(u, rest);
    if (instance_token !== undefined) u.has_instance_token = instance_token !== "";
    return ok(u);
  },
  adminDeleteUpstream: (ctx) => {
    const u = ctx.state.upstreams.find((x) => x.id === ctx.params.id);
    if (u?.source === "config")
      return problem(409, "This Upstream comes from the configuration file. Disable it there.");
    ctx.state.upstreams = ctx.state.upstreams.filter((x) => x.id !== ctx.params.id);
    return noContent;
  },
  adminTestUpstream: (ctx) => {
    const u = ctx.state.upstreams.find((x) => x.id === ctx.params.id);
    if (!u) return problem(404, "Upstream not found.");
    return u.enabled
      ? ok({ name: u.name, ok: true, message: `Connected to ${u.base_url}.`, latency_ms: 142 })
      : ok({
          name: u.name,
          ok: false,
          message: `Could not connect to ${u.base_url}: connection refused.`,
        });
  },
  adminGetSettings: ({ state }) => ok(state.settings),
  adminUpdateSettings: (ctx) => {
    const p = b(ctx) as S["InstanceSettings"];
    for (const w of p.pull_windows ?? []) {
      if (!/^\d{2}:\d{2}-\d{2}:\d{2}$/.test(w)) {
        return problem(400, `Pull window ${w} is not in the form 01:00-07:00.`, [
          { pointer: "/pull_windows", detail: "Use HH:MM-HH:MM." },
        ]);
      }
    }
    const { oidc_providers: _ro, ...rest } = p;
    Object.assign(ctx.state.settings, rest);
    return ok(ctx.state.settings);
  },
  adminGetStorage: ({ state }) => {
    const byNs = new Map<string, { lfs: number; git: number; count: number }>();
    for (const r of state.repos) {
      const e = byNs.get(r.repo.namespace) ?? { lfs: 0, git: 0, count: 0 };
      e.lfs += r.repo.lfs_bytes;
      e.git += r.repo.size_bytes - r.repo.lfs_bytes;
      e.count++;
      byNs.set(r.repo.namespace, e);
    }
    const namespaces = [...byNs.entries()].map(([namespace, e]) => ({
      namespace,
      lfs_bytes: e.lfs,
      git_bytes: e.git,
      repo_count: e.count,
      ...(state.quotas[namespace] ? { quota_bytes: state.quotas[namespace] } : {}),
    }));
    return ok({
      total_lfs_bytes: namespaces.reduce((a, n) => a + n.lfs_bytes, 0),
      total_git_bytes: namespaces.reduce((a, n) => a + n.git_bytes, 0),
      xet_cache_bytes: 734_003_200,
      namespaces,
      last_gc_at: iso(6 * 3_600_000),
    });
  },
  adminSetQuota: (ctx) => {
    const limit = Number(b(ctx).bytes_limit ?? 0);
    if (limit === 0) delete ctx.state.quotas[ctx.params.ns ?? ""];
    else ctx.state.quotas[ctx.params.ns ?? ""] = limit;
    return ok({ bytes_limit: limit });
  },
  adminRunGC: ({ state }) => ok(job(state, "gc", { scope: "all" }, "pending"), 202),
  adminListAudit: (ctx) => {
    const actor = ctx.query.get("actor");
    const action = ctx.query.get("action");
    // W5's filter: a comma-separated list of exact actions (internal/audit).
    const actions = (action ?? "").split(",").filter(Boolean);
    const items = ctx.state.audit.filter(
      (a) => (!actor || a.actor === actor) && (!actions.length || actions.includes(a.action)),
    );
    return ok(page(ctx, items));
  },
  adminGetSystem: () =>
    ok({
      version: "1.0.0-dev",
      commit: "b3ed701",
      role: "all",
      database: { driver: "sqlite", schema_version: "0001" },
      blob_store: { backend: "fs" },
      telemetry: { traces: "none", metrics: "prometheus", logs: "none" },
      checks: [
        {
          name: "database",
          ok: true,
          message: "Connected; migrations are current.",
          latency_ms: 2,
        },
        { name: "blob_store", ok: true, message: "Read and wrote a test object.", latency_ms: 5 },
        { name: "git", ok: true, message: "git 2.47.1 found." },
        {
          name: "upstream:huggingface",
          ok: false,
          message: "huggingface.co did not answer within 5 seconds.",
        },
      ],
    }),
  adminListTokens: (ctx) =>
    ok(
      page(
        ctx,
        ctx.state.tokens.map((t) => ({ ...t, username: ctx.state.me.username })),
      ),
    ),
  adminRevokeToken: (ctx) => {
    ctx.state.tokens = ctx.state.tokens.filter((t) => t.id !== ctx.params.id);
    return noContent;
  },
};

function commitInfo(c: MockCommit): S["CommitInfo"] {
  const sig = {
    name: c.author,
    email: `${c.author}@example.com`,
    date: c.date,
    username: c.author,
  };
  return {
    sha: c.sha,
    parents: c.parents,
    author: sig,
    committer: sig,
    title: c.title,
    message: c.message,
  };
}

function uploadStatus(id: string, u: MockState["uploads"][string]): S["UploadStatus"] {
  return {
    id,
    sha256: u.sha256,
    size: u.size,
    part_size: u.partSize,
    part_count: Math.max(1, Math.ceil(u.size / u.partSize)),
    parts: u.parts,
    state: u.state,
  };
}

function tree(ctx: Ctx, r: MockRepo, path: string): Result {
  const c = resolveRev(r, ctx.params.rev);
  if (!c) return problem(404, `Revision ${ctx.params.rev} does not exist.`);
  const file = c.files.find((f) => f.path === path);
  if (file) return ok({ commit: c.sha, items: [treeEntry(r, c, file)] });
  const items = listTree(r, c, path, ctx.query.get("recursive") === "true");
  if (!items) return problem(404, `${path} does not exist at ${ctx.params.rev}.`);
  return ok({ commit: c.sha, ...page(ctx, items, 1000) });
}

function findCollection(ctx: Ctx): S["Collection"] | undefined {
  return ctx.state.collections.find(
    (c) => c.namespace === ctx.params.ns && c.slug === ctx.params.slug,
  );
}

function expandCollection(state: MockState) {
  return (c: S["Collection"]): S["Collection"] => ({
    ...c,
    items: [...c.items]
      .sort((a, b2) => a.position - b2.position)
      .map((i) => {
        const r =
          i.type === "collection"
            ? undefined
            : findRepo(state, i.type, ...(i.item_id.split("/") as [string, string]));
        return r ? { ...i, repo: r.repo } : i;
      }),
  });
}

export { fileOid, lfsOid, uuidOf };
