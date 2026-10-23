import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PageShell } from "@/components/PageShell";
import { repoPageTitle } from "@/features/repos/RepoLayout";
import { documentTitle, focusMainHeading } from "./page";

describe("page titles", () => {
  afterEach(cleanup);

  it("names the browser tab after the page, then Hangar", () => {
    expect(documentTitle("Tokens")).toBe("Tokens · Hangar");
    expect(documentTitle("")).toBe("Hangar");
    expect(documentTitle()).toBe("Hangar");
  });

  it("sets the title from the page heading, and puts it back on leaving", () => {
    const { unmount, rerender } = render(<PageShell title="Tokens">x</PageShell>);
    expect(document.title).toBe("Tokens · Hangar");
    rerender(
      <PageShell title={<span>Ada Lovelace</span>} documentTitle="Ada Lovelace">
        x
      </PageShell>,
    );
    expect(document.title).toBe("Ada Lovelace · Hangar");
    unmount();
    expect(document.title).toBe("Hangar");
  });

  it("names Repo pages after the Repo ID and what they show", () => {
    expect(repoPageTitle("ada/m", "model", "", {})).toBe("ada/m");
    expect(repoPageTitle("ada/imdb", "dataset", "/viewer", {})).toBe("Viewer · datasets/ada/imdb");
    expect(repoPageTitle("ada/m", "model", "/tree/main", {})).toBe("Files · ada/m");
    expect(
      repoPageTitle("ada/m", "model", "/blob/refs/pr/1/docs/use.md", { _splat: "docs/use.md" }),
    ).toBe("use.md · ada/m");
    expect(repoPageTitle("ada/m", "model", "/commit/abc", { sha: "0123456789abcdef" })).toBe(
      "Commit 0123456 · ada/m",
    );
    expect(repoPageTitle("ada/m", "model", "/discussions/4", { num: "4" })).toBe(
      "Discussion #4 · ada/m",
    );
    expect(repoPageTitle("ada/m", "model", "/settings", {})).toBe("Settings · ada/m");
  });
});

describe("focus after navigation", () => {
  afterEach(cleanup);

  it("focuses the page's h1, or main while there is none", () => {
    const { rerender } = render(
      <main id="main" tabIndex={-1}>
        <p>Loading</p>
      </main>,
    );
    focusMainHeading();
    expect(screen.getByRole("main")).toHaveFocus();
    rerender(
      <main id="main" tabIndex={-1}>
        <h1 id="page-title">Tokens</h1>
      </main>,
    );
    focusMainHeading();
    const heading = screen.getByRole("heading", { name: "Tokens" });
    expect(heading).toHaveFocus();
    expect(heading).toHaveAttribute("tabindex", "-1");
  });
});
