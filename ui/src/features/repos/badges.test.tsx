import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { QuantChips } from "./badges";

afterEach(cleanup);

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
