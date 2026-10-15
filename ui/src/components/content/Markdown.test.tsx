import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { LazyMarkdown } from "./LazyMarkdown";
import Markdown, { isExternal, tableLabel } from "./Markdown";

describe("Markdown cards", () => {
  it("removes scripts, event handlers and iframes", () => {
    const { container } = render(
      <Markdown>
        {
          '# Card\n\n<script>alert(1)</script>\n\n<img src="/a.png" onerror="alert(2)" alt="a">\n\n<iframe src="https://evil.example"></iframe>\n\n<div align="center">kept</div>'
        }
      </Markdown>,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("iframe")).toBeNull();
    expect(container.querySelector("img")?.getAttribute("onerror")).toBeNull();
    expect(screen.getByText("kept")).toHaveAttribute("align", "center");
    expect(screen.getByRole("heading", { name: "Card" })).toBeInTheDocument();
  });

  it("nests a card's headings under the page's h1", () => {
    const { container } = render(
      <Markdown>{"# bert-tiny-finetune\n\n## Usage\n\n### Notes\n\n<h1>Raw</h1>"}</Markdown>,
    );
    // 09-web-ui.md: one h1 per page, the page title. The card's own "# "
    // heading must not become a second one.
    expect(container.querySelector("h1")).toBeNull();
    expect(screen.getByRole("heading", { level: 2, name: "bert-tiny-finetune" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 3, name: "Usage" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 4, name: "Notes" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "Raw" })).toBeVisible();
  });

  it("starts headings at the level it is given and keeps their distances", () => {
    const { container, rerender } = render(
      <Markdown headingLevel={3}>{"# Title\n\n###### Deep"}</Markdown>,
    );
    expect(screen.getByRole("heading", { level: 3, name: "Title" })).toBeVisible();
    // Below h6 there is nothing, so the deepest headings stay h6.
    expect(screen.getByRole("heading", { level: 6, name: "Deep" })).toBeVisible();
    // A card that starts at "###" moves up, so the outline has no gap.
    rerender(<Markdown>{"### Only\n\n#### Under"}</Markdown>);
    expect(screen.getByRole("heading", { level: 2, name: "Only" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 3, name: "Under" })).toBeVisible();
    expect(container.querySelector("h1")).toBeNull();
  });

  it("renders GitHub tables and links external sites safely", () => {
    render(
      <Markdown>
        {"| a | b |\n|---|---|\n| 1 | 2 |\n\n[site](https://example.com) [local](/ada/m)"}
      </Markdown>,
    );
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "site" })).toHaveAttribute(
      "rel",
      "noopener noreferrer nofollow",
    );
    expect(screen.getByRole("link", { name: "local" })).not.toHaveAttribute("target");
  });

  it("puts each table in its own focusable scroll region", () => {
    render(
      <Markdown>
        {
          "| Metric | Value |\n|---|---|\n| Accuracy | 0.81 |\n\n| Split | Rows |\n|---|---|\n| train | 10 |"
        }
      </Markdown>,
    );
    const first = screen.getByRole("region", { name: "Table: Metric, Value" });
    const second = screen.getByRole("region", { name: "Table: Split, Rows" });
    // axe's scrollable-region-focusable: keyboard users scroll a wide table
    // by focusing its region, so the table itself must not scroll.
    expect(first).toHaveAttribute("tabindex", "0");
    expect(second).toHaveAttribute("tabindex", "0");
    expect(first.querySelector("table")).toHaveTextContent("Accuracy");
    expect(tableLabel(undefined)).toBe("Table");
  });

  it("does not load external images when the Instance turns them off", () => {
    render(
      <Markdown externalImages={false}>
        {"![chart](https://example.com/chart.png) ![local](/x.png)"}
      </Markdown>,
    );
    expect(screen.queryByRole("img", { name: "chart" })).toBeNull();
    expect(screen.getByText(/Image from example.com not loaded/)).toBeInTheDocument();
    expect(screen.getByRole("img", { name: "local" })).toHaveAttribute("src", "/x.png");
  });

  it("resolves relative links and images inside the Repo", () => {
    render(
      <Markdown resolveRelative={(p, k) => `/${k}/${p}`}>
        {"![plot](img/plot.png) [docs](docs/use.md) [anchor](#top)"}
      </Markdown>,
    );
    expect(screen.getByRole("img", { name: "plot" })).toHaveAttribute("src", "/image/img/plot.png");
    expect(screen.getByRole("link", { name: "docs" })).toHaveAttribute("href", "/link/docs/use.md");
    expect(screen.getByRole("link", { name: "anchor" })).toHaveAttribute("href", "#top");
    expect(isExternal("https://other.example/x", "http://localhost")).toBe(true);
    expect(isExternal("/x", "http://localhost")).toBe(false);
  });
});

describe("LazyMarkdown", () => {
  it("announces a loading status until the Markdown code loads", async () => {
    render(<LazyMarkdown>{"# Loaded card"}</LazyMarkdown>);
    expect(screen.getByRole("status", { name: "Loading Markdown" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Loaded card" })).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: "Loading Markdown" })).toBeNull();
  });
});
