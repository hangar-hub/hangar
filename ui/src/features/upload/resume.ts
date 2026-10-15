/**
 * Remembers which upload holds the parts of each object, so an upload can
 * resume after a page reload (09-web-ui.md "Upload"). The upload plan
 * continues the user's upload of the same object in the Repo, but can start
 * a new one, for example against an older server, so the browser keeps the
 * ID of the first one per Repo and SHA-256 and asks the server which of its
 * parts exist.
 */

const prefix = "hangar-upload:";

/** How long a remembered upload is kept before it is dropped unasked. */
export const resumeMaxAge = 7 * 24 * 60 * 60 * 1000;

export type RepoKey = { type: "models" | "datasets"; ns: string; name: string };

export type RememberedUpload = { id: string; size: number; savedAt: number };

type Store = Pick<Storage, "getItem" | "setItem" | "removeItem" | "key" | "length">;

/** The browser's localStorage, or undefined where it is blocked or missing. */
function defaultStore(): Store | undefined {
  try {
    return globalThis.localStorage ?? undefined;
  } catch {
    return undefined;
  }
}

function keyOf(repo: RepoKey, sha256: string): string {
  return `${prefix}${repo.type}/${repo.ns}/${repo.name}/${sha256}`;
}

function parse(raw: string | null): RememberedUpload | undefined {
  if (!raw) return undefined;
  try {
    const v = JSON.parse(raw) as Partial<RememberedUpload>;
    if (typeof v.id === "string" && typeof v.size === "number" && typeof v.savedAt === "number") {
      return { id: v.id, size: v.size, savedAt: v.savedAt };
    }
  } catch {
    // A value that is not ours is ignored.
  }
  return undefined;
}

/** The upload remembered for an object in a Repo, if any and not too old. */
export function rememberedUpload(
  repo: RepoKey,
  sha256: string,
  now: number = Date.now(),
  store: Store | undefined = defaultStore(),
): RememberedUpload | undefined {
  try {
    const v = parse(store?.getItem(keyOf(repo, sha256)) ?? null);
    if (v && now - v.savedAt <= resumeMaxAge) return v;
  } catch {
    // Storage can throw when site data is blocked; resuming is then skipped.
  }
  return undefined;
}

/** Remembers the upload for an object, and drops remembered uploads past their age. */
export function rememberUpload(
  repo: RepoKey,
  sha256: string,
  id: string,
  size: number,
  now: number = Date.now(),
  store: Store | undefined = defaultStore(),
): void {
  if (!store) return;
  try {
    const stale: string[] = [];
    for (let i = 0; i < store.length; i++) {
      const k = store.key(i);
      if (!k?.startsWith(prefix)) continue;
      const v = parse(store.getItem(k));
      if (!v || now - v.savedAt > resumeMaxAge) stale.push(k);
    }
    for (const k of stale) store.removeItem(k);
    store.setItem(keyOf(repo, sha256), JSON.stringify({ id, size, savedAt: now }));
  } catch {
    // Storage can be full or blocked; the upload still works, it just cannot resume.
  }
}

/** Forgets the upload for an object, once it is complete or gone. */
export function forgetUpload(
  repo: RepoKey,
  sha256: string,
  store: Store | undefined = defaultStore(),
): void {
  try {
    store?.removeItem(keyOf(repo, sha256));
  } catch {
    // Nothing to do when storage is blocked.
  }
}
