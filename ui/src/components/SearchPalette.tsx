import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { Command } from "cmdk";
import {
  Boxes,
  Database,
  FolderPlus,
  KeyRound,
  Library,
  Search,
  Telescope,
  User,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { api } from "@/api/client";
import { useGo } from "@/components/AppLink";
import { Dialog } from "@/components/ui/dialog";
import { unwrap } from "@/lib/problem";
import { repoLabel, repoPath } from "@/lib/repo";

/** True when the key event comes from a field the user is typing in. */
export function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.isContentEditable;
}

/** True for the shortcuts that open search: `/`, Cmd-K and Ctrl-K. */
export function isSearchShortcut(
  e: Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "target">,
): boolean {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") return true;
  return e.key === "/" && !e.metaKey && !e.ctrlKey && !isTyping(e.target);
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

const itemClass =
  "flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 text-sm aria-selected:bg-raised [&_svg]:size-4 [&_svg]:text-muted";

/**
 * The command palette over Repos, Namespaces, Collections and actions
 * (BROWSE-6). Opens with `/` or Cmd-K.
 */
export function SearchPalette() {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const query = useDebounced(q.trim(), 150);
  const navigateTo = useGo();
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (isSearchShortcut(e)) {
        e.preventDefault();
        setOpen(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const results = useQuery({
    queryKey: ["search", query],
    enabled: open && query.length > 0,
    queryFn: async () =>
      unwrap(await api.GET("/search", { params: { query: { q: query, limit: 8 } } })),
    // Keep the last results on screen while the next query loads, so the
    // list does not empty and refill on every key press.
    placeholderData: keepPreviousData,
  });
  // An empty query shows only the actions, not the last query's results.
  const data = query ? results.data : undefined;
  const found = data
    ? data.repos.length + data.namespaces.length + data.collections.length
    : undefined;

  const go = (to: string) => {
    setOpen(false);
    setQ("");
    navigateTo(to);
  };

  const allActions = [
    { label: "New model", to: "/new", icon: <FolderPlus /> },
    { label: "New dataset", to: "/new-dataset", icon: <Database /> },
    { label: "Discover models on an Upstream", to: "/discover", icon: <Telescope /> },
    { label: "Open your arsenal", to: "/arsenal", icon: <Boxes /> },
    { label: "Collections", to: "/collections", icon: <Library /> },
    { label: "Create a Token", to: "/settings/tokens", icon: <KeyRound /> },
  ];
  const actions = allActions.filter(
    (a) => !query || a.label.toLowerCase().includes(query.toLowerCase()),
  );
  const status =
    query && results.isFetching ? "Searching" : found === 0 ? `No matches for ${query}.` : "";

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md border border-line bg-bg px-2.5 text-sm text-muted hover:border-line-strong sm:max-w-xs"
      >
        <Search className="size-4 shrink-0" aria-hidden="true" />
        <span className="truncate">Search Hangar</span>
        <kbd className="ml-auto hidden rounded-sm border border-line px-1 font-mono text-[11px] sm:inline">
          /
        </kbd>
      </button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Search"
        description="Repos, Namespaces, Collections and actions."
        initialFocus={input}
      >
        <Command label="Search Hangar" shouldFilter={false} className="-mx-1">
          <Command.Input
            ref={input}
            value={q}
            onValueChange={setQ}
            placeholder="Search by name, such as llama or ml-team"
            aria-label="Search"
            className="mb-2 h-9 w-full rounded-md border border-line-strong bg-surface px-3 text-sm"
          />
          {/* Outside the list: a listbox may only contain options and groups
              of options (axe aria-required-children), so the loading and
              empty messages are a status beside it. */}
          <p role="status" className="px-2 text-sm text-muted">
            {status}
          </p>
          <Command.List className="max-h-[50vh] overflow-y-auto">
            {data && data.repos.length > 0 ? (
              <Command.Group
                heading="Repos"
                className="text-xs text-muted [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1"
              >
                {data.repos.map((r) => (
                  <Command.Item
                    key={`${r.type}/${r.id}`}
                    value={`repo:${r.type}:${r.id}`}
                    onSelect={() => go(repoPath(r.type, r.namespace, r.name))}
                    className={itemClass}
                  >
                    {r.type === "dataset" ? <Database /> : <Boxes />}
                    <span className="font-mono text-ink">
                      {repoLabel(r.type, r.namespace, r.name)}
                    </span>
                  </Command.Item>
                ))}
              </Command.Group>
            ) : null}
            {data && data.namespaces.length > 0 ? (
              <Command.Group
                heading="Namespaces"
                className="text-xs text-muted [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1"
              >
                {data.namespaces.map((n) => (
                  <Command.Item
                    key={n.name}
                    value={`ns:${n.name}`}
                    onSelect={() => go(`/${n.name}`)}
                    className={itemClass}
                  >
                    <User />
                    <span className="text-ink">{n.display_name || n.name}</span>
                    <span className="font-mono text-xs">{n.name}</span>
                  </Command.Item>
                ))}
              </Command.Group>
            ) : null}
            {data && data.collections.length > 0 ? (
              <Command.Group
                heading="Collections"
                className="text-xs text-muted [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1"
              >
                {data.collections.map((c) => (
                  <Command.Item
                    key={`${c.namespace}/${c.slug}`}
                    value={`col:${c.namespace}/${c.slug}`}
                    onSelect={() => go(`/collections/${c.namespace}/${c.slug}`)}
                    className={itemClass}
                  >
                    <Library />
                    <span className="text-ink">{c.title}</span>
                    <span className="font-mono text-xs">{c.namespace}</span>
                  </Command.Item>
                ))}
              </Command.Group>
            ) : null}
            {actions.length > 0 ? (
              <Command.Group
                heading="Actions"
                className="text-xs text-muted [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1"
              >
                {actions.map((a) => (
                  <Command.Item
                    key={a.to}
                    value={`action:${a.to}`}
                    onSelect={() => go(a.to)}
                    className={itemClass}
                  >
                    {a.icon}
                    <span className="text-ink">{a.label}</span>
                  </Command.Item>
                ))}
              </Command.Group>
            ) : null}
          </Command.List>
        </Command>
      </Dialog>
    </>
  );
}
