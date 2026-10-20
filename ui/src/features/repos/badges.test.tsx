import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { QuantChips, ScanBadge } from "./badges";

afterEach(() => {
  cleanup();
  document.head.querySelector('meta[name="docs-url"]')?.remove();
});

function setDocsURL(url: string) {
  const meta = document.createElement("meta");
  meta.name = "docs-url";
  meta.content = url;
  document.head.append(meta);
}

describe("QuantChips", () => {
  it("caps the chips and names the rest in the +N chip", () => {
    render(<QuantChips quants={["Q2_K", "Q4_K_M", "Q5_K_M", "Q6_K", "Q8_0"]} max={3} />);
    expect(screen.getByText("Q2_K")).toBeInTheDocument();
    expect(screen.getByText("Q5_K_M")).toBeInTheDocument();
    expect(screen.queryByText("Q6_K")).not.toBeInTheDocument();
    const more = screen.getByRole("img", { name: "2 more quants: Q6_K, Q8_0" });
    expect(more).toHaveTextContent("+2");
    expect(more).toHaveAttribute("title", "2 more quants: Q6_K, Q8_0");
  });

  it("shows every chip when they fit", () => {
    render(<QuantChips quants={["Q4_K_M", "Q8_0"]} max={3} />);
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.getByText("Q8_0")).toBeInTheDocument();
  });

  it("says quant for one more", () => {
    render(<QuantChips quants={["Q2_K", "Q4_K_M"]} max={1} />);
    expect(screen.getByRole("img", { name: "1 more quant: Q4_K_M" })).toHaveTextContent("+1");
  });
});

describe("ScanBadge", () => {
  it("explains a Repo's unsafe imports when clicked, and closes on Escape", async () => {
    const user = userEvent.setup();
    render(<ScanBadge status="unsafe" />);
    const badge = screen.getByRole("button", { name: "Unsafe imports" });
    await user.click(badge);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("at least one file in this Repo");
    expect(dialog).toHaveTextContent("os.system");
    expect(screen.getByRole("link", { name: "Read more in the docs" })).toHaveAttribute(
      "href",
      "https://hangar.page/operate/security/#choose-what-to-block",
    );
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("opens from the keyboard", async () => {
    const user = userEvent.setup();
    render(<ScanBadge status="suspicious" scope="file" />);
    await user.tab();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("dialog")).toHaveTextContent("imports in this file");
  });

  it("stays a plain badge where the page already explains it", () => {
    render(<ScanBadge status="unsafe" plain />);
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.getByText("Unsafe imports")).toBeInTheDocument();
  });
});

describe("docs links in explanations", () => {
  it("go to the docs mirror the Instance names", async () => {
    setDocsURL("https://docs.internal/hangar");
    const user = userEvent.setup();
    render(<ScanBadge status="unsafe" />);
    await user.click(screen.getByRole("button", { name: "Unsafe imports" }));
    expect(await screen.findByRole("link", { name: "Read more in the docs" })).toHaveAttribute(
      "href",
      "https://docs.internal/hangar/operate/security/#choose-what-to-block",
    );
  });

  it("are left out when the Instance turns them off", async () => {
    setDocsURL("");
    const user = userEvent.setup();
    render(<ScanBadge status="unsafe" />);
    await user.click(screen.getByRole("button", { name: "Unsafe imports" }));
    expect(await screen.findByRole("dialog")).toHaveTextContent("os.system");
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });
});
