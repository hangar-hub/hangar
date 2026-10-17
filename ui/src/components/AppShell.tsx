import { Link, useNavigate } from "@tanstack/react-router";
import { LogOut, Menu, Settings, Shield, User } from "lucide-react";
import { type ReactNode, useState } from "react";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import {
  MenuContent,
  MenuItem,
  MenuLabel,
  MenuRoot,
  MenuSeparator,
  MenuTrigger,
} from "@/components/ui/menu";
import { useToast } from "@/components/ui/toast";
import { errorMessage } from "@/lib/problem";
import { useMe, useSignOut } from "@/lib/session";
import { JobsIndicator } from "./JobsDrawer";
import { Logo } from "./Logo";
import { SearchPalette } from "./SearchPalette";
import { ThemeToggle } from "./ThemeToggle";

const nav = [
  { to: "/models", label: "Models" },
  { to: "/datasets", label: "Datasets" },
  { to: "/discover", label: "Discover" },
  { to: "/collections", label: "Collections" },
] as const;

function UserMenu() {
  const me = useMe();
  const signOut = useSignOut();
  const navigate = useNavigate();
  const toast = useToast();
  if (me.isPending) return <div className="h-8 w-16" aria-hidden="true" />;
  if (!me.data) {
    return (
      <Button asChild size="sm" variant="primary">
        <Link to="/auth/login">Sign in</Link>
      </Button>
    );
  }
  const user = me.data;
  return (
    <MenuRoot>
      <MenuTrigger asChild>
        <Button variant="ghost" size="sm" aria-label={`Account menu for ${user.username}`}>
          <span className="grid size-6 place-items-center rounded-sm bg-steel-700 font-mono text-xs uppercase text-steel-50">
            {user.username.slice(0, 1)}
          </span>
        </Button>
      </MenuTrigger>
      <MenuContent>
        <MenuLabel>
          Signed in as <span className="font-mono text-ink">{user.username}</span>
        </MenuLabel>
        <MenuItem asChild>
          <Link to="/$ns" params={{ ns: user.username }}>
            <User aria-hidden="true" />
            Your profile
          </Link>
        </MenuItem>
        <MenuItem asChild>
          <Link to="/settings/tokens">
            <Settings aria-hidden="true" />
            Settings
          </Link>
        </MenuItem>
        {user.is_admin ? (
          <MenuItem asChild>
            <Link to="/admin">
              <Shield aria-hidden="true" />
              Admin
            </Link>
          </MenuItem>
        ) : null}
        <MenuSeparator />
        <MenuItem
          onSelect={async () => {
            try {
              await signOut();
              navigate({ to: "/" });
            } catch (e) {
              toast(errorMessage(e), "danger");
            }
          }}
        >
          <LogOut aria-hidden="true" />
          Sign out
        </MenuItem>
      </MenuContent>
    </MenuRoot>
  );
}

function MobileNav() {
  const [open, setOpen] = useState(false);
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      side="left"
      title="Hangar"
      trigger={
        <Button variant="ghost" size="icon-sm" className="md:hidden" aria-label="Open navigation">
          <Menu aria-hidden="true" />
        </Button>
      }
    >
      <nav aria-label="Main" className="flex flex-col gap-1">
        {nav.map((item) => (
          <Link
            key={item.to}
            to={item.to}
            onClick={() => setOpen(false)}
            className="rounded-md px-3 py-2 hover:bg-raised"
            activeProps={{ className: "bg-raised font-semibold" }}
          >
            {item.label}
          </Link>
        ))}
        <Link
          to="/settings/tokens"
          onClick={() => setOpen(false)}
          className="rounded-md px-3 py-2 hover:bg-raised"
        >
          Settings
        </Link>
      </nav>
    </Dialog>
  );
}

/** The global layout: top bar with name, search, navigation, Jobs and user menu, then the page. */
export function AppShell({ children }: { children: ReactNode }) {
  const me = useMe();
  return (
    <div className="flex min-h-screen flex-col bg-bg text-ink">
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:left-2 focus:top-2 focus:z-50 focus:rounded-md focus:bg-surface focus:p-2"
      >
        Skip to content
      </a>
      <header className="sticky top-0 z-30 border-b border-line bg-surface/95 backdrop-blur">
        <div className="mx-auto flex h-12 max-w-[1440px] items-center gap-2 px-4 sm:gap-3">
          <MobileNav />
          <Link
            to="/"
            className="flex shrink-0 items-center gap-2 font-semibold tracking-tight"
            aria-label="Hangar home"
          >
            <Logo className="size-6" />
            <span className="hidden sm:inline">Hangar</span>
          </Link>
          <nav aria-label="Main" className="hidden gap-0.5 md:flex">
            {nav.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                className="rounded-md px-2.5 py-1 text-sm text-muted hover:bg-raised hover:text-ink"
                activeProps={{ className: "!text-ink font-semibold bg-raised" }}
              >
                {item.label}
              </Link>
            ))}
          </nav>
          <div className="flex min-w-0 flex-1 justify-end md:justify-center">
            <SearchPalette />
          </div>
          <div className="flex shrink-0 items-center gap-1">
            {me.data ? <JobsIndicator /> : null}
            <ThemeToggle />
            <UserMenu />
          </div>
        </div>
      </header>
      <main id="main" className="mx-auto w-full max-w-[1440px] flex-1 px-4 py-6">
        {children}
      </main>
      <footer className="border-t border-line py-4">
        <p className="mx-auto max-w-[1440px] px-4 text-xs text-muted">
          Hangar is source-available under the Elastic License 2.0.
        </p>
      </footer>
    </div>
  );
}
