import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef } from "react";
import { describe, expect, it, vi } from "vitest";
import { DiffView, diffLines } from "@/features/files/DiffView";
import { ApiError } from "@/lib/problem";
import { DemoBanner, docsUrl, installUrl, isDemo } from "./DemoBanner";
import { PageShell } from "./PageShell";
import { isSearchShortcut } from "./SearchPalette";
import { Dialog } from "./ui/dialog";
import { EmptyState, ErrorState, PermissionDenied } from "./ui/states";
import { DataTable } from "./ui/table";

describe("states", () => {
  it("shows the problem detail with a retry", async () => {
    const retry = vi.fn();
    render(
      <ErrorState
        error={
          new ApiError(500, { type: "t", title: "t", status: 500, detail: "The database is down." })
        }
        onRetry={retry}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("The database is down.");
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it("shows permission denied for 403 and sign in for 401", () => {
    const { unmount } = render(<ErrorState error={new ApiError(403)} />);
    expect(screen.getByText("You don't have access to this")).toBeInTheDocument();
    unmount();
    render(<ErrorState error={new ApiError(401)} />);
    expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute(
      "href",
      expect.stringContaining("/auth/login?redirect="),
    );
  });

  it("renders empty states and page frames", () => {
    render(
      <PageShell title="Arsenal" subtitle="All of it">
        <EmptyState title="Your arsenal is empty.">
          Discover a model to pull, or upload your own.
        </EmptyState>
        <PermissionDenied>Admins only.</PermissionDenied>
      </PageShell>,
    );
    expect(screen.getByRole("heading", { level: 1, name: "Arsenal" })).toBeInTheDocument();
    expect(screen.getByText("Discover a model to pull, or upload your own.")).toBeInTheDocument();
    expect(screen.getByText("Admins only.")).toBeInTheDocument();
  });
});

describe("Dialog", () => {
  it("focuses the first focusable element, or the one it is given", () => {
    const { unmount } = render(
      <Dialog open title="Plain">
        <input aria-label="Field" />
      </Dialog>,
    );
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
    unmount();
    // The search palette opens with focus in its input, so typing searches.
    function WithInitialFocus() {
      const ref = useRef<HTMLInputElement>(null);
      return (
        <Dialog open title="Search" initialFocus={ref}>
          <input ref={ref} aria-label="Search Hangar" />
        </Dialog>
      );
    }
    render(<WithInitialFocus />);
    expect(screen.getByRole("textbox", { name: "Search Hangar" })).toHaveFocus();
  });

  it("closes a dialog that shows a secret once only with its button", async () => {
    const change = vi.fn();
    const done = vi.fn();
    render(
      <Dialog
        open
        persistent
        onOpenChange={change}
        title="Copy your new Token"
        footer={
          <button type="button" onClick={done}>
            I've copied it
          </button>
        }
      >
        <code>hgr_secret</code>
      </Dialog>,
    );
    expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
    await userEvent.keyboard("{Escape}");
    fireEvent.pointerDown(document.body);
    expect(change).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Copy your new Token" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "I've copied it" }));
    expect(done).toHaveBeenCalledOnce();
  });

  it("closes an ordinary dialog with Escape", async () => {
    const change = vi.fn();
    render(
      <Dialog open onOpenChange={change} title="Plain">
        <input aria-label="Field" />
      </Dialog>,
    );
    await userEvent.keyboard("{Escape}");
    expect(change).toHaveBeenCalledWith(false);
  });
});

describe("DataTable", () => {
  it("keeps table semantics and card labels", () => {
    render(
      <DataTable
        caption="Files"
        rows={[{ n: "a.txt", s: 1 }]}
        rowKey={(r) => r.n}
        columns={[
          { key: "n", header: "Name", primary: true, cell: (r) => r.n },
          { key: "s", header: "Size", cell: (r) => `${r.s} B` },
        ]}
      />,
    );
    expect(screen.getByRole("table", { name: "Files" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Size" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: /1 B/ })).toBeInTheDocument();
  });
});

describe("DiffView", () => {
  it("classifies lines and keeps the signs", () => {
    expect(
      diffLines("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n same").map((l) => l.kind),
    ).toEqual(["meta", "meta", "hunk", "del", "add", "ctx"]);
    render(<DiffView patch={"-old\n+new"} label="Diff of a.txt" />);
    expect(screen.getByRole("region", { name: "Diff of a.txt" })).toBeInTheDocument();
    expect(screen.getByText("-old")).toBeInTheDocument();
    expect(screen.getByText("+new")).toBeInTheDocument();
  });
});

describe("search shortcut", () => {
  it("opens on / outside fields and on Cmd-K or Ctrl-K anywhere", () => {
    const input = document.createElement("input");
    expect(
      isSearchShortcut({ key: "/", metaKey: false, ctrlKey: false, target: document.body }),
    ).toBe(true);
    expect(isSearchShortcut({ key: "/", metaKey: false, ctrlKey: false, target: input })).toBe(
      false,
    );
    expect(isSearchShortcut({ key: "k", metaKey: true, ctrlKey: false, target: input })).toBe(true);
    expect(
      isSearchShortcut({ key: "K", metaKey: false, ctrlKey: true, target: document.body }),
    ).toBe(true);
    expect(
      isSearchShortcut({ key: "a", metaKey: false, ctrlKey: false, target: document.body }),
    ).toBe(false);
  });
});

describe("DemoBanner", () => {
  it("says the data is sample data and links to the docs and the install guide", () => {
    render(<DemoBanner />);
    const banner = screen.getByRole("region", { name: "Demo" });
    expect(banner).toHaveTextContent(
      "This is a demo with sample data. Nothing you do here is saved. Read the documentation or install Hangar.",
    );
    expect(screen.getByRole("link", { name: "install Hangar" })).toHaveAttribute(
      "href",
      installUrl,
    );
    expect(installUrl).toBe("https://hangar.page/latest/get-started/");
    expect(screen.getByRole("link", { name: "Read the documentation" })).toHaveAttribute(
      "href",
      "https://hangar.page/",
    );
    expect(docsUrl).toBe("https://hangar.page/");
  });

  it("is off unless the build sets VITE_HANGAR_DEMO=1", () => {
    // make ui and the tests build without the flag; make demo sets it.
    expect(import.meta.env.VITE_HANGAR_DEMO).toBeUndefined();
    expect(isDemo).toBe(false);
  });
});
