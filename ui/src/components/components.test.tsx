import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { DiffView, diffLines } from "@/features/files/DiffView";
import { ApiError } from "@/lib/problem";
import { PageShell } from "./PageShell";
import { isSearchShortcut } from "./SearchPalette";
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
    render(<DiffView patch={"-old\n+new"} />);
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
