import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { ThemeToggle } from "./ThemeToggle";

const nav = [
  { to: "/arsenal", label: "Arsenal" },
  { to: "/discover", label: "Discover" },
  { to: "/collections", label: "Collections" },
] as const;

/** The global layout: top bar with name, navigation and theme, then the page. */
export function AppShell({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-screen bg-white text-neutral-900 dark:bg-neutral-950 dark:text-neutral-100">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:p-2">
        Skip to content
      </a>
      <header className="border-b border-neutral-200 dark:border-neutral-800">
        <div className="mx-auto flex max-w-7xl items-center gap-4 px-4 py-2">
          <Link to="/" className="flex items-center gap-2 font-semibold">
            <img src="/assets/hangar.svg" alt="" className="h-6 w-6" />
            Hangar
          </Link>
          <nav aria-label="Main" className="flex gap-1">
            {nav.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                className="rounded-md px-2 py-1 text-sm hover:bg-neutral-100 dark:hover:bg-neutral-800"
                activeProps={{ className: "font-semibold text-accent-700 dark:text-accent-500" }}
              >
                {item.label}
              </Link>
            ))}
          </nav>
          <div className="ml-auto">
            <ThemeToggle />
          </div>
        </div>
      </header>
      <main id="main" className="mx-auto max-w-7xl px-4 py-6">
        {children}
      </main>
    </div>
  );
}
