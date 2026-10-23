import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@/components/ui/toast";
import { FileEditor } from "./BlobPage";

const repo = { kind: "model" as const, ns: "ada", name: "m" };

function wrap(children: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <ToastProvider>{children}</ToastProvider>
    </QueryClientProvider>
  );
}

describe("FileEditor", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("commits against the Commit editing started from, and explains a conflict", async () => {
    const bodies: Record<string, unknown>[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: Request) => {
        bodies.push((await input.json()) as Record<string, unknown>);
        return new Response(
          JSON.stringify({
            type: "about:blank",
            title: "Precondition Failed",
            status: 412,
            detail: "Parent commit is not the head of main.",
          }),
          { status: 412, headers: { "Content-Type": "application/problem+json" } },
        );
      }),
    );
    const editor = (parent: string) => (
      <FileEditor
        repo={repo}
        branch="main"
        path="notes.txt"
        initial="hello"
        parent={parent}
        creating={false}
        onDone={() => {}}
      />
    );
    const { rerender } = render(wrap(editor("a".repeat(40))));
    // A background refetch brings a newer head after someone else commits.
    // The editor must keep the Commit it started from, or the server's
    // conflict check would pass and their change would be overwritten.
    rerender(wrap(editor("b".repeat(40))));
    await userEvent.click(screen.getByRole("button", { name: "Commit to main" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Someone committed to main after you started editing, so Hangar did not save your change.",
    );
    expect(bodies).toHaveLength(1);
    expect(bodies[0]?.parent_commit).toBe("a".repeat(40));
    expect(bodies[0]?.branch).toBe("main");
  });
});
