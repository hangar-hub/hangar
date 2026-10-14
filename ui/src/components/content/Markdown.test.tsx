import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { LazyMarkdown } from "./LazyMarkdown";
import Markdown, { isExternal } from "./Markdown";

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
